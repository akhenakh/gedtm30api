package geotiff

import (
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

	"golang.org/x/sync/singleflight"
)

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

type VRTGeo struct {
	imageWidth  int
	imageLength int
	PixelScaleX float64
	PixelScaleY float64
	originX     float64
	originY     float64
	sources     []VRTSourceInfo

	readerFactory ReaderFactory
	sourceReaders map[string]*GeoTIFF
	mu            sync.Mutex

	inflight singleflight.Group
}

func OpenVRT(r io.Reader, readerFactory ReaderFactory, cacheSize int64, itemsToPrune uint32) (*VRTGeo, error) {
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

	return &VRTGeo{
		imageWidth:    int(math.Round(vrt.RasterXSize)),
		imageLength:   int(math.Round(vrt.RasterYSize)),
		PixelScaleX:   geoTransform[1],
		PixelScaleY:   pixelScaleY,
		originX:       geoTransform[0],
		originY:       geoTransform[3],
		sources:       sources,
		readerFactory: readerFactory,
		sourceReaders: make(map[string]*GeoTIFF),
	}, nil
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

func (v *VRTGeo) getSourceGeo(filename string) (*GeoTIFF, error) {
	v.mu.Lock()
	if geo, ok := v.sourceReaders[filename]; ok {
		v.mu.Unlock()
		return geo, nil
	}
	v.mu.Unlock()

	vgeo, err, _ := v.inflight.Do(filename, func() (interface{}, error) {
		v.mu.Lock()
		if geo, ok := v.sourceReaders[filename]; ok {
			v.mu.Unlock()
			return geo, nil
		}
		v.mu.Unlock()

		slog.Debug("VRT opening source GeoTIFF", "filename", filename)
		reader, err := v.readerFactory(context.Background(), filename)
		if err != nil {
			return nil, fmt.Errorf("failed to create reader for source %s: %w", filename, err)
		}

		geo, err := Open(reader, 256, 50)
		if err != nil {
			return nil, fmt.Errorf("failed to open source GeoTIFF %s: %w", filename, err)
		}
		geo.Name = filename

		v.mu.Lock()
		v.sourceReaders[filename] = geo
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

			x1, y1 := v.coordToPixel(startLon, startLat, bounds)
			x2, y2 := v.coordToPixel(endLon, endLat, bounds)

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

func (v *VRTGeo) pixelToCoord(x, y int, bounds *CornerCoordinates) (float64, float64) {
	lon := bounds.UpperLeft.Lon + (float64(x) * v.PixelScaleX)
	lat := bounds.UpperLeft.Lat + (float64(y) * v.PixelScaleY)
	return lon, lat
}
