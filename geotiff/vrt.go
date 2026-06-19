package geotiff

import (
	"container/list"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/karlseguin/ccache/v3"
	"golang.org/x/sync/singleflight"
)

// defaultMaxOpenSources bounds how many source GeoTIFF handles a VRT keeps open
// at once when no explicit limit is configured.
const defaultMaxOpenSources = 256

type vrtXML struct {
	RasterXSize  float64 `xml:"rasterXSize,attr"`
	RasterYSize  float64 `xml:"rasterYSize,attr"`
	GeoTransform string  `xml:"GeoTransform"`
	Bands        []vrtBandXML `xml:"VRTRasterBand"`
}

type vrtBandXML struct {
	DataType       string               `xml:"dataType,attr"`
	Band           int                  `xml:"band,attr"`
	SimpleSources  []vrtSourceXML       `xml:"SimpleSource"`
	ComplexSources []vrtSourceXML       `xml:"ComplexSource"`
}

type vrtSourceXML struct {
	SourceFilename vrtFilenameXML `xml:"SourceFilename"`
	SourceBand     int            `xml:"SourceBand"`
	SrcRect        vrtRectXML     `xml:"SrcRect"`
	DstRect        vrtRectXML     `xml:"DstRect"`
}

type vrtFilenameXML struct {
	Value         string `xml:",chardata"`
	RelativeToVRT int    `xml:"relativeToVRT,attr"`
}

type vrtRectXML struct {
	XOff  float64 `xml:"xOff,attr"`
	YOff  float64 `xml:"yOff,attr"`
	XSize float64 `xml:"xSize,attr"`
	YSize float64 `xml:"ySize,attr"`
}

type VRTSourceInfo struct {
	Filename  string
	Relative  bool // relativeToVRT attribute
	SourceBand int
	SrcXOff, SrcYOff, SrcXSize, SrcYSize int
	DstXOff, DstYOff, DstXSize, DstYSize int
}

type ReaderFactory func(ctx context.Context, filename string) (io.ReadSeeker, error)

func resolveVRTFilename(baseURL, filename string, relative bool) string {
	if filename == "" {
		return filename
	}

	// GDAL virtual filesystem prefixes
	if rest, ok := strings.CutPrefix(filename, "/vsicurl/"); ok {
		return rest
	}
	if strings.HasPrefix(filename, "/vsi") {
		idx := strings.Index(filename[1:], "/")
		if idx >= 0 {
			return filename[idx+2:]
		}
		return filename
	}

	if !relative {
		return filename
	}

	if baseURL == "" {
		return filename
	}

	if idx := strings.LastIndex(baseURL, "/"); idx >= 0 {
		return baseURL[:idx+1] + filename
	}
	return baseURL + "/" + filename
}

type VRTGeo struct {
	imageWidth  int
	imageLength int
	PixelScaleX float64
	PixelScaleY float64
	originX     float64
	originY     float64
	sources     []VRTSourceInfo
	vrtBaseURL  string

	readerFactory ReaderFactory

	// tileCache is shared by every source GeoTIFF so MaxSize bounds the total
	// number of cached tiles across the whole VRT (see GeoTIFF.cacheKeyPrefix).
	tileCache *ccache.Cache[any]

	// sourceReaders / sourceLRU form a count-bounded LRU of open source
	// handles. Evicted sources are dropped here; their libtiff handles are
	// reclaimed by GeoTIFF.finalizeRemote once no read still references them.
	sourceReaders map[string]*list.Element
	sourceLRU     *list.List
	maxSources    int
	mu            sync.Mutex

	inflight singleflight.Group
}

// sourceEntry is the value stored in the source LRU list.
type sourceEntry struct {
	filename string
	geo      *GeoTIFF
}

func OpenVRT(r io.Reader, readerFactory ReaderFactory, cacheSize int64, itemsToPrune uint32, maxOpenSources int) (*VRTGeo, error) {
	var vrt vrtXML
	if err := xml.NewDecoder(r).Decode(&vrt); err != nil {
		return nil, fmt.Errorf("failed to parse VRT XML: %w", err)
	}

	raw := strings.NewReplacer(",", " ").Replace(vrt.GeoTransform)
	parts := strings.Fields(raw)
	if len(parts) != 6 {
		return nil, fmt.Errorf("invalid GeoTransform: expected 6 values, got %d", len(parts))
	}
	geoTransform := make([]float64, 6)
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid GeoTransform value at position %d: %w", i, err)
		}
		geoTransform[i] = v
	}

	var sources []VRTSourceInfo
	for _, band := range vrt.Bands {
		for _, src := range band.SimpleSources {
			sources = append(sources, VRTSourceInfo{
				Filename:   strings.TrimSpace(src.SourceFilename.Value),
				Relative:   src.SourceFilename.RelativeToVRT == 1,
				SourceBand: src.SourceBand,
				SrcXOff:    int(math.Round(src.SrcRect.XOff)),
				SrcYOff:    int(math.Round(src.SrcRect.YOff)),
				SrcXSize:   int(math.Round(src.SrcRect.XSize)),
				SrcYSize:   int(math.Round(src.SrcRect.YSize)),
				DstXOff:    int(math.Round(src.DstRect.XOff)),
				DstYOff:    int(math.Round(src.DstRect.YOff)),
				DstXSize:   int(math.Round(src.DstRect.XSize)),
				DstYSize:   int(math.Round(src.DstRect.YSize)),
			})
		}
		for _, src := range band.ComplexSources {
			sources = append(sources, VRTSourceInfo{
				Filename:   strings.TrimSpace(src.SourceFilename.Value),
				Relative:   src.SourceFilename.RelativeToVRT == 1,
				SourceBand: src.SourceBand,
				SrcXOff:    int(math.Round(src.SrcRect.XOff)),
				SrcYOff:    int(math.Round(src.SrcRect.YOff)),
				SrcXSize:   int(math.Round(src.SrcRect.XSize)),
				SrcYSize:   int(math.Round(src.SrcRect.YSize)),
				DstXOff:    int(math.Round(src.DstRect.XOff)),
				DstYOff:    int(math.Round(src.DstRect.YOff)),
				DstXSize:   int(math.Round(src.DstRect.XSize)),
				DstYSize:   int(math.Round(src.DstRect.YSize)),
			})
		}
	}

	if len(sources) == 0 {
		return nil, errors.New("VRT contains no source files")
	}

	pixelScaleY := geoTransform[5]
	if pixelScaleY > 0 {
		pixelScaleY = -pixelScaleY
	}

	slog.Debug("VRT parsed", "rasterXSize", vrt.RasterXSize, "rasterYSize", vrt.RasterYSize, "pixelScaleX", geoTransform[1], "pixelScaleY", pixelScaleY, "originX", geoTransform[0], "originY", geoTransform[3], "numSources", len(sources))

	if maxOpenSources <= 0 {
		maxOpenSources = defaultMaxOpenSources
	}

	return &VRTGeo{
		imageWidth:    int(math.Round(vrt.RasterXSize)),
		imageLength:   int(math.Round(vrt.RasterYSize)),
		PixelScaleX:   geoTransform[1],
		PixelScaleY:   pixelScaleY,
		originX:       geoTransform[0],
		originY:       geoTransform[3],
		sources:       sources,
		readerFactory: readerFactory,
		tileCache:     ccache.New(ccache.Configure[any]().MaxSize(cacheSize).ItemsToPrune(itemsToPrune)),
		sourceReaders: make(map[string]*list.Element),
		sourceLRU:     list.New(),
		maxSources:    maxOpenSources,
	}, nil
}

// lruGet returns a cached source and marks it most-recently-used.
// The caller must hold v.mu.
func (v *VRTGeo) lruGet(filename string) (*GeoTIFF, bool) {
	if el, ok := v.sourceReaders[filename]; ok {
		v.sourceLRU.MoveToFront(el)
		return el.Value.(*sourceEntry).geo, true
	}
	return nil, false
}

// lruPut inserts a source as most-recently-used and evicts the
// least-recently-used handles past maxSources. The caller must hold v.mu.
func (v *VRTGeo) lruPut(filename string, geo *GeoTIFF) {
	if el, ok := v.sourceReaders[filename]; ok {
		el.Value.(*sourceEntry).geo = geo
		v.sourceLRU.MoveToFront(el)
		return
	}
	v.sourceReaders[filename] = v.sourceLRU.PushFront(&sourceEntry{filename: filename, geo: geo})

	for v.sourceLRU.Len() > v.maxSources {
		back := v.sourceLRU.Back()
		if back == nil {
			break
		}
		ent := back.Value.(*sourceEntry)
		v.sourceLRU.Remove(back)
		delete(v.sourceReaders, ent.filename)
		slog.Debug("VRT evicting source GeoTIFF from LRU", "filename", ent.filename, "open", v.sourceLRU.Len())
		// The handle is released by ent.geo's finalizer once no in-flight read
		// still references it; cached tiles persist in the shared tile cache.
	}
}

func (v *VRTGeo) AtCoord(lon, lat float64) (float32, error) {
	bounds, err := v.Bounds()
	if err != nil {
		return 0, err
	}
	p := Point{Lon: lon, Lat: lat}
	if !bounds.Contains(p) {
		slog.Debug("VRT AtCoord point outside bounds", "lon", lon, "lat", lat, "bounds", bounds.String())
		return 0, fmt.Errorf("requested point %s does not fall inside the image bounds %s", p.String(), bounds.String())
	}

	x := int(math.Round(math.Abs(p.Lon-bounds.UpperLeft.Lon) / v.PixelScaleX))
	y := int(math.Round(math.Abs(p.Lat-bounds.UpperLeft.Lat) / math.Abs(v.PixelScaleY)))

	return v.loc(x, y)
}

func (v *VRTGeo) Bounds() (*CornerCoordinates, error) {
	totalWidth := float64(v.imageWidth) * v.PixelScaleX
	totalHeight := float64(v.imageLength) * v.PixelScaleY

	return &CornerCoordinates{
		UpperLeft:  Point{Lon: v.originX, Lat: v.originY},
		LowerLeft:  Point{Lon: v.originX, Lat: v.originY + totalHeight},
		UpperRight: Point{Lon: v.originX + totalWidth, Lat: v.originY},
		LowerRight: Point{Lon: v.originX + totalWidth, Lat: v.originY + totalHeight},
	}, nil
}

func (v *VRTGeo) loc(x, y int) (float32, error) {
	if x < 0 || x >= v.imageWidth || y < 0 || y >= v.imageLength {
		return 0, errors.New("point lies outside image")
	}

	src, err := v.findSource(x, y)
	if err != nil {
		return 0, err
	}

	sx := src.SrcXOff + (x-src.DstXOff)*src.SrcXSize/src.DstXSize
	sy := src.SrcYOff + (y-src.DstYOff)*src.SrcYSize/src.DstYSize

	srcGeo, err := v.getSourceGeo(src.Filename)
	if err != nil {
		return 0, err
	}

	srcBounds, err := srcGeo.Bounds()
	if err != nil {
		return 0, err
	}
	srcLon := srcBounds.UpperLeft.Lon + float64(sx)*srcGeo.PixelScaleX
	srcLat := srcBounds.UpperLeft.Lat + float64(sy)*srcGeo.PixelScaleY

	slog.Debug("VRT mapping pixel to source", "vrtPixelX", x, "vrtPixelY", y, "sourceFile", src.Filename, "srcPixelX", sx, "srcPixelY", sy, "srcLon", srcLon, "srcLat", srcLat)
	return srcGeo.AtCoord(srcLon, srcLat)
}

func (v *VRTGeo) findSource(x, y int) (*VRTSourceInfo, error) {
	for i := range v.sources {
		s := &v.sources[i]
		if x >= s.DstXOff && x < s.DstXOff+s.DstXSize &&
			y >= s.DstYOff && y < s.DstYOff+s.DstYSize {
			return s, nil
		}
	}
	slog.Debug("VRT no source covers pixel", "pixelX", x, "pixelY", y, "numSources", len(v.sources), "imageWidth", v.imageWidth, "imageLength", v.imageLength)
	return nil, fmt.Errorf("no source covers pixel (%d, %d)", x, y)
}

func (v *VRTGeo) SetBaseURL(baseURL string) {
	v.vrtBaseURL = baseURL
}

func (v *VRTGeo) findSourceByFilename(filename string) *VRTSourceInfo {
	for i := range v.sources {
		if v.sources[i].Filename == filename {
			return &v.sources[i]
		}
	}
	return nil
}

func (v *VRTGeo) getSourceGeo(filename string) (*GeoTIFF, error) {
	v.mu.Lock()
	if geo, ok := v.lruGet(filename); ok {
		v.mu.Unlock()
		return geo, nil
	}
	v.mu.Unlock()

	vgeo, err, _ := v.inflight.Do(filename, func() (interface{}, error) {
		v.mu.Lock()
		if geo, ok := v.lruGet(filename); ok {
			v.mu.Unlock()
			return geo, nil
		}
		v.mu.Unlock()

		src := v.findSourceByFilename(filename)
		resolved := filename
		if src != nil && v.vrtBaseURL != "" {
			resolved = resolveVRTFilename(v.vrtBaseURL, filename, src.Relative)
		}

		slog.Debug("VRT opening source GeoTIFF", "filename", filename, "resolved", resolved)
		reader, err := v.readerFactory(context.Background(), resolved)
		if err != nil {
			return nil, fmt.Errorf("failed to create reader for source %s: %w", filename, err)
		}

		// Share the VRT's tile cache, namespaced by resolved path, so the
		// cache budget bounds tiles across all sources rather than per source.
		geo, err := OpenWithCache(reader, v.tileCache, resolved+"#")
		if err != nil {
			return nil, fmt.Errorf("failed to open source GeoTIFF %s: %w", filename, err)
		}
		geo.Name = resolved

		v.mu.Lock()
		v.lruPut(filename, geo)
		v.mu.Unlock()

		return geo, nil
	})

	if err != nil {
		return nil, err
	}
	return vgeo.(*GeoTIFF), nil
}

func (v *VRTGeo) Profile(coordinates [][]float64) ([][]float64, error) {
	if len(coordinates) < 2 {
		return nil, errors.New("at least two coordinate pairs are required to create a profile")
	}

	bounds, err := v.Bounds()
	if err != nil {
		return nil, err
	}

	slog.Debug("VRT profile start", "numCoords", len(coordinates), "bounds", bounds.String())

	var profile [][]float64
	visitedPixels := make(map[string]struct{})

	for i := 0; i < len(coordinates)-1; i++ {
		startCoords := coordinates[i]
		endCoords := coordinates[i+1]

		if len(startCoords) != 2 || len(endCoords) != 2 {
			return nil, fmt.Errorf("invalid coordinate pair at index %d; expected [lat, lng]", i)
		}

		startLat, startLon := startCoords[0], startCoords[1]
		endLat, endLon := endCoords[0], endCoords[1]

		x1, y1, err := v.coordToPixelChecked(startLon, startLat, bounds)
		if err != nil {
			slog.Debug("VRT profile: start point outside bounds, skipping segment", "seg", i, "lat", startLat, "lon", startLon, "error", err)
			continue
		}
		x2, y2, err := v.coordToPixelChecked(endLon, endLat, bounds)
		if err != nil {
			slog.Debug("VRT profile: end point outside bounds, skipping segment", "seg", i, "lat", endLat, "lon", endLon, "error", err)
			continue
		}

			slog.Debug("VRT profile segment", "seg", i, "startLat", startLat, "startLon", startLon, "endLat", endLat, "endLon", endLon, "startPx", x1, "startPy", y1, "endPx", x2, "endPy", y2)

		dx := float64(x2 - x1)
		dy := float64(y2 - y1)

		steps := math.Max(math.Abs(dx), math.Abs(dy))
		numSteps := int(math.Ceil(steps))
		if numSteps == 0 {
			numSteps = 1
		}

		xInc := dx / float64(numSteps)
		yInc := dy / float64(numSteps)

		for j := 0; j <= numSteps; j++ {
			currX := x1 + int(float64(j)*xInc)
			currY := y1 + int(float64(j)*yInc)

			pixelKey := fmt.Sprintf("%d,%d", currX, currY)
			if _, ok := visitedPixels[pixelKey]; ok {
				continue
			}
			visitedPixels[pixelKey] = struct{}{}

			elevation, err := v.loc(currX, currY)
			if err != nil {
				slog.Debug("VRT profile: could not get elevation for pixel, skipping", "pixelX", currX, "pixelY", currY, "error", err)
				continue
			}

			lon, lat := v.pixelToCoord(currX, currY, bounds)

			profile = append(profile, []float64{lat, lon, float64(elevation)})
		}
	}

	return profile, nil
}

func (v *VRTGeo) coordToPixel(lon, lat float64, bounds *CornerCoordinates) (int, int) {
	x := int(math.Round(math.Abs(lon-bounds.UpperLeft.Lon) / v.PixelScaleX))
	y := int(math.Round(math.Abs(lat-bounds.UpperLeft.Lat) / math.Abs(v.PixelScaleY)))
	return x, y
}

func (v *VRTGeo) coordToPixelChecked(lon, lat float64, bounds *CornerCoordinates) (int, int, error) {
	if !bounds.Contains(Point{Lon: lon, Lat: lat}) {
		return 0, 0, fmt.Errorf("point (Lon: %f, Lat: %f) is outside the image bounds", lon, lat)
	}
	x, y := v.coordToPixel(lon, lat, bounds)
	return x, y, nil
}

func (v *VRTGeo) pixelToCoord(x, y int, bounds *CornerCoordinates) (float64, float64) {
	lon := bounds.UpperLeft.Lon + (float64(x) * v.PixelScaleX)
	lat := bounds.UpperLeft.Lat + (float64(y) * v.PixelScaleY)
	return lon, lat
}
