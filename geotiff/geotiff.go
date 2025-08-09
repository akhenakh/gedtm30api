package geotiff

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/karlseguin/ccache/v3"
	"golang.org/x/sync/singleflight"
)

// head represents the TIFF file header information
type head struct {
	byteOrder binary.ByteOrder // Byte order (little endian or big endian)
	isBigTIFF bool             // Whether this is a BigTIFF file format
	ifdOffset uint64           // Offset to the first Image File Directory (IFD)
}

// iFDEntry represents a single entry in an Image File Directory (IFD)
type iFDEntry struct {
	Tag         Tag       // TIFF tag identifier
	FType       fieldType // Data type of the field
	Count       uint64    // Number of values of the specified type
	ValueOffset uint64    // Offset to the value data, or the value itself if it fits inline
	ValueBytes  []byte    // Inline value data for small values
}

// tagData holds the parsed data for a TIFF tag in various typed formats
type tagData struct {
	fType      fieldType // The field type of this tag data
	length     uint32    // Number of elements in the data
	byteData   []uint8   // Raw byte data (BYTE type)
	asciiData  string    // String data (ASCII type)
	shortData  []uint16  // 16-bit unsigned integer data (SHORT type)
	longData   []uint32  // 32-bit unsigned integer data (LONG type)
	floatData  []float32 // 32-bit floating point data (FLOAT type)
	doubleData []float64 // 64-bit floating point data (DOUBLE type)
	uint64Data []uint64  // 64-bit unsigned integer data (LONG8/IFD8 types)
}

type Tags map[Tag]tagData

// GeoTIFF represents a parsed GeoTIFF file with its metadata and data access capabilities
type GeoTIFF struct {
	// reader is the underlying source for the GeoTIFF data. It must implement
	// io.ReadSeeker and io.ReaderAt for efficient access, especially for remote files.
	reader io.ReadSeeker

	// byteOrder stores the endianness (little or big) of the TIFF file,
	// which is critical for correctly interpreting binary data.
	byteOrder binary.ByteOrder

	// tags holds all the parsed metadata from the Image File Directory (IFD)
	// as a map from TIFF tag IDs to their data.
	tags Tags

	// isBigTIFF is a flag indicating whether the file uses the BigTIFF format,
	// which supports 64-bit offsets for files larger than 4GB.
	isBigTIFF bool

	// imageWidth is the total width of the image in pixels.
	imageWidth uint32
	// imageLength is the total height (length) of the image in pixels.
	imageLength uint32

	// tileWidth is the width of a single tile in pixels. Tiled TIFFs are
	// a core feature of Cloud Optimized GeoTIFFs (COGs).
	tileWidth uint32
	// tileLength is the height (length) of a single tile in pixels.
	tileLength uint32

	// tileOffsets stores the file offset for the beginning of each tile's data.
	tileOffsets []uint64
	// tileByteCounts stores the size in bytes of each compressed tile.
	tileByteCounts []uint64

	// bitsPerSample indicates the number of bits per data sample (e.g., 32 for float32).
	bitsPerSample uint16
	// sampleFormat specifies the data type of the samples (e.g., float, signed/unsigned int).
	sampleFormat uint16
	// compression indicates the compression method used for tile data (e.g., DEFLATE).
	compression uint16
	// predictor specifies a prediction scheme used before compression to improve ratios.
	predictor uint16

	// PixelScaleX is the scaling factor for converting pixel coordinates to geographic
	// coordinates in the X (longitude) direction.
	PixelScaleX float64
	// PixelScaleY is the scaling factor for converting pixel coordinates to geographic
	// coordinates in the Y (latitude) direction. Usually negative for north-up images.
	PixelScaleY float64

	// tileCache is an in-memory LRU cache that stores *processed* tile data.
	// Instead of caching raw bytes, it caches the final, ready-to-use data slices
	// (e.g., []float32), which dramatically reduces CPU load and garbage collection
	// pressure on cache hits. The value type is `any` to accommodate different
	// sample formats (e.g. []float32, []int32).
	tileCache *ccache.Cache[any]

	// inflightData uses a singleflight.Group to prevent "thundering herd" problems.
	// It ensures that for a given tile, only one goroutine will perform the I/O
	// and processing, while other concurrent requests for the same tile wait for
	// the result.
	inflightData singleflight.Group

	// inflightPrefetch uses a separate singleflight.Group to ensure that the prefetching
	// logic for a given tile's neighbors is only triggered once, avoiding redundant
	// prefetch operations initiated by concurrent requests.
	inflightPrefetch singleflight.Group

	// tilesAcross is a pre-calculated value for the number of tiles in the horizontal
	// direction, used to quickly compute a tile's index from its X/Y coordinates.
	tilesAcross int
}

type Point struct{ Lon, Lat float64 }

type CornerCoordinates struct{ UpperLeft, LowerLeft, UpperRight, LowerRight Point }

type Tag uint16

// fieldTypeLen is the length of every field type in bytes
var fieldTypeLen = [...]uint32{
	zeroByte, oneByte, oneByte, twoByte, // 0-3
	fourByte, eightByte, oneByte, oneByte, // 4-7
	twoByte, fourByte, eightByte, fourByte, // 8-11
	eightByte, // 12 (DOUBLE)
	0, 0, 0,   // 13-15 (Reserved)
	eightByte, eightByte, eightByte, // 16-18 (LONG8, SLONG8, IFD8)
}

var fieldTypeToLabel = map[fieldType]string{
	BYTE:      "BYTE",
	ASCII:     "ASCII",
	SHORT:     "SHORT",
	LONG:      "LONG",
	RATIONAL:  "RATIONAL",
	SBYTE:     "SBYTE",
	UNDEFINED: "UNDEFINED",
	SSHORT:    "SSHORT",
	SLONG:     "SLONG",
	SRATIONAL: "SRATIONAL",
	FLOAT:     "FLOAT",
	DOUBLE:    "DOUBLE",
}

func (f fieldType) String() string {
	v, ok := fieldTypeToLabel[f]
	if !ok {
		return fmt.Sprintf("unrecognized field type %d", f)
	}
	return v
}

// bytes returns the number of bytes in each data type
//
// returns 0 if unrecognized
func (f fieldType) bytes() uint32 {
	if f == 0 || int(f) > len(fieldTypeLen) {
		return fieldTypeLen[0]
	}
	return fieldTypeLen[int(f)]
}

func (t Tag) String() string {
	v, ok := tagToLabel[t]
	if !ok {
		return fmt.Sprintf("%d", t)
	}
	return v
}

// Open parses a GeoTIFF file from the provided io.ReadSeeker and returns a GeoTIFF struct
// with all necessary metadata extracted for reading geographic raster data.
func Open(r io.ReadSeeker, cacheSize int64, itemsToPrune uint32) (*GeoTIFF, error) {
	// Read and parse all TIFF tags from the file header
	gTags, header, err := readTags(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read tiff tags: %w", err)
	}

	// Initialize the GeoTIFF struct with basic file information

	g := &GeoTIFF{
		reader:    r,
		tags:      gTags,
		byteOrder: header.byteOrder,
		isBigTIFF: header.isBigTIFF,
		// Cache to hold processed slices.
		tileCache: ccache.New(ccache.Configure[any]().MaxSize(cacheSize).ItemsToPrune(itemsToPrune)),
	}

	// Extract required image dimensions
	var ok bool
	if width, ok := g.getUint(ImageWidth); ok {
		g.imageWidth = uint32(width)
	} else {
		return nil, errors.New("missing or invalid tag: ImageWidth")
	}
	if length, ok := g.getUint(ImageLength); ok {
		g.imageLength = uint32(length)
	} else {
		return nil, errors.New("missing or invalid tag: ImageLength")
	}

	// Extract required tile dimensions
	if tWidth, ok := g.getUint(TileWidth); ok {
		g.tileWidth = uint32(tWidth)
	} else {
		return nil, errors.New("missing or invalid tag: TileWidth")
	}
	if tLength, ok := g.getUint(TileLength); ok {
		g.tileLength = uint32(tLength)
	} else {
		return nil, errors.New("missing or invalid tag: TileLength")
	}

	// Pre-calculate number of tiles across for later use
	if g.tileWidth > 0 {
		g.tilesAcross = int(g.imageWidth+g.tileWidth-1) / int(g.tileWidth)
	}

	// Extract sample format information with defaults
	if bps, ok := g.getUint(BitsPerSample); ok {
		g.bitsPerSample = uint16(bps)
	} else {
		g.bitsPerSample = 32 // Default to 32-bit samples
	}
	if sf, ok := g.getUint(SampleFormat); ok {
		g.sampleFormat = uint16(sf)
	} else {
		g.sampleFormat = SampleFormatFloat // Default to floating point format
	}

	// Extract compression settings with defaults
	if comp, ok := g.getUint(Compression); ok {
		g.compression = uint16(comp)
	} else {
		g.compression = Uncompressed // Default to uncompressed if tag is missing
	}

	if pred, ok := g.getUint(Predictor); ok {
		g.predictor = uint16(pred)
	} else {
		g.predictor = PredictorNone // Default if tag is missing
	}

	// Extract tile location and size information (required for tiled images)
	if offsets, ok := g.get64bitSlice(TileOffsets); ok {
		g.tileOffsets = offsets
	} else {
		return nil, errors.New("missing or invalid tag: TileOffsets")
	}
	if counts, ok := g.get64bitSlice(TileByteCounts); ok {
		g.tileByteCounts = counts
	} else {
		return nil, errors.New("missing or invalid tag: TileByteCounts")
	}

	// Extract geographic pixel scale information
	pixelScale, ok := gTags[ModelPixelScale]
	if !ok {
		return nil, errors.New("missing tag: ModelPixelScale")
	}
	pixelScaleValues, _ := pixelScale.doubleDataValue()
	g.PixelScaleX = pixelScaleValues[0] // Geographic units per pixel in X direction
	g.PixelScaleY = pixelScaleValues[1] // Geographic units per pixel in Y direction

	// Ensure Y pixel scale is negative (standard GeoTIFF convention for north-up images)
	if g.PixelScaleY > 0 {
		g.PixelScaleY = -g.PixelScaleY
	}

	return g, nil
}

// AtCoord returns the raster value at the specified longitude and latitude coordinates.
// It first checks if the coordinates fall within the image bounds, then converts the
// geographic coordinates to pixel indices and retrieves the value at that location.
func (g *GeoTIFF) AtCoord(lon, lat float64) (float32, error) {
	// Get the geographic bounds of the image
	rect, err := g.Bounds()
	if err != nil {
		return 0, err
	}

	// Create a point from the input coordinates
	p := Point{Lon: lon, Lat: lat}

	// Check if the requested point falls within the image bounds
	if !rect.Contains(p) {
		return 0, fmt.Errorf("requested point %s does not fall inside the image bounds %s", p.String(), rect.String())
	}

	// Convert longitude to pixel index (X direction)
	// Calculate distance from upper-left corner and divide by pixel scale
	xIDx := int(math.Round(math.Abs(p.Lon-rect.UpperLeft.Lon) / g.PixelScaleX))

	// Convert latitude to pixel index (Y direction)
	// Calculate distance from upper-left corner and divide by pixel scale
	yIDx := int(math.Round(math.Abs(p.Lat-rect.UpperLeft.Lat) / math.Abs(g.PixelScaleY)))

	// Retrieve the raster value at the calculated pixel indices
	return g.loc(xIDx, yIDx)
}

func (g *GeoTIFF) Bounds() (*CornerCoordinates, error) {
	tiePointTag, ok := g.tags[ModelTiepoint]
	if !ok {
		return nil, errors.New("missing ModelTiepoint tag")
	}
	tiePointValues, ok := tiePointTag.doubleDataValue()
	if !ok || len(tiePointValues) < 6 {
		return nil, errors.New("invalid ModelTiepoint tag")
	}

	tieI, tieJ := tiePointValues[0], tiePointValues[1]
	tieLon, tieLat := tiePointValues[3], tiePointValues[4]

	// Calculate the coordinate of the upper-left corner.
	ulLon := tieLon - (tieI * g.PixelScaleX)
	ulLat := tieLat - (tieJ * g.PixelScaleY)

	// Calculate the full extent of the image in geographic units.
	// g.PixelScaleY is now guaranteed to be negative.
	totalWidth := float64(g.imageWidth) * g.PixelScaleX
	totalHeight := float64(g.imageLength) * g.PixelScaleY // This will be a negative value

	// Construct the corner coordinates.
	cc := &CornerCoordinates{
		UpperLeft:  Point{Lon: ulLon, Lat: ulLat},
		LowerLeft:  Point{Lon: ulLon, Lat: ulLat + totalHeight},
		UpperRight: Point{Lon: ulLon + totalWidth, Lat: ulLat},
		LowerRight: Point{Lon: ulLon + totalWidth, Lat: ulLat + totalHeight},
	}
	return cc, nil
}

// readHeader parses the TIFF file header to determine byte order, file format, and IFD location
func readHeader(r io.Reader) (head, error) {
	var h head

	// Read the first 2 bytes to determine byte order (little or big endian)
	var byteOrderBytes uint16
	if err := binary.Read(r, binary.BigEndian, &byteOrderBytes); err != nil {
		return h, err
	}

	// Set the byte order based on the magic bytes
	switch byteOrderBytes {
	case littleEndian:
		h.byteOrder = binary.LittleEndian
	case bigEndian:
		h.byteOrder = binary.BigEndian
	default:
		return h, errors.New("invalid byte order")
	}

	// Read the TIFF identifier to determine if this is standard TIFF or BigTIFF
	var identifier uint16
	if err := binary.Read(r, h.byteOrder, &identifier); err != nil {
		return h, err
	}

	// Process based on TIFF format type
	switch identifier {
	case tiffIdentifier:
		// Standard TIFF format - uses 32-bit offsets
		h.isBigTIFF = false
		var offset32 uint32
		if err := binary.Read(r, h.byteOrder, &offset32); err != nil {
			return h, err
		}
		h.ifdOffset = uint64(offset32)
	case bigTiffIdentifier:
		// BigTIFF format - uses 64-bit offsets for large files
		h.isBigTIFF = true

		// Read and validate the bytesize field (should be 8 for BigTIFF)
		var bytesize, reserved uint16
		if err := binary.Read(r, h.byteOrder, &bytesize); err != nil {
			return h, err
		}
		if bytesize != bigTiffBytesize {
			return h, errors.New("invalid BigTIFF bytesize")
		}

		// Read the reserved field (should be 0)
		if err := binary.Read(r, h.byteOrder, &reserved); err != nil {
			return h, err
		}

		// Read the 64-bit IFD offset
		if err := binary.Read(r, h.byteOrder, &h.ifdOffset); err != nil {
			return h, err
		}
	default:
		return h, fmt.Errorf("invalid tiff identifier: %d", identifier)
	}
	return h, nil
}

func readTags(r io.ReadSeeker) (Tags, head, error) {
	tags := make(Tags)
	h, err := readHeader(r)
	if err != nil {
		return nil, h, err
	}

	// For a COG, we only want the first IFD, which contains the full-resolution image.
	// We will not loop to subsequent IFDs (which are overviews).
	ifdOffset := h.ifdOffset
	if ifdOffset == 0 {
		return nil, h, errors.New("file contains no IFDs")
	}

	// Seek to the one and only IFD we need to read
	if _, err := r.Seek(int64(ifdOffset), io.SeekStart); err != nil {
		return nil, h, err
	}

	var numEntries uint64
	if h.isBigTIFF {
		if err := binary.Read(r, h.byteOrder, &numEntries); err != nil {
			return nil, h, err
		}
	} else {
		var numEntries16 uint16
		if err := binary.Read(r, h.byteOrder, &numEntries16); err != nil {
			return nil, h, err
		}
		numEntries = uint64(numEntries16)
	}

	entryLen := 12
	if h.isBigTIFF {
		entryLen = 20
	}
	ifdBlockSize := (entryLen * int(numEntries))
	ifdBlock := make([]byte, ifdBlockSize)
	if _, err := io.ReadFull(r, ifdBlock); err != nil {
		return nil, h, fmt.Errorf("failed to read IFD block: %w", err)
	}
	ifdReader := bytes.NewReader(ifdBlock)

	for i := uint64(0); i < numEntries; i++ {
		var entry iFDEntry
		var tag, ftype uint16
		binary.Read(ifdReader, h.byteOrder, &tag)
		binary.Read(ifdReader, h.byteOrder, &ftype)
		entry.Tag = Tag(tag)
		entry.FType = fieldType(ftype)
		if entry.FType.bytes() == 0 {
			log.Printf("Warning: unrecognized tag %d with field type %d. Skipping.", entry.Tag, entry.FType)
			ifdReader.Seek(int64(entryLen-4), io.SeekCurrent)
			continue
		}

		offsetBytes := make([]byte, 8)
		if h.isBigTIFF {
			binary.Read(ifdReader, h.byteOrder, &entry.Count)
			ifdReader.Read(offsetBytes)
			entry.ValueOffset = h.byteOrder.Uint64(offsetBytes)
		} else {
			var count32, offset32 uint32
			binary.Read(ifdReader, h.byteOrder, &count32)
			binary.Read(ifdReader, h.byteOrder, &offset32)
			entry.Count = uint64(count32)
			entry.ValueOffset = uint64(offset32)
			// For inline data compatibility, put the 4-byte value/offset into the 8-byte slice
			h.byteOrder.PutUint32(offsetBytes, offset32)
		}

		inlineDataSize := uint64(4)
		if h.isBigTIFF {
			inlineDataSize = 8
		}

		if totalBytes := uint64(entry.FType.bytes()) * entry.Count; totalBytes <= inlineDataSize {
			entry.ValueBytes = offsetBytes[:totalBytes]
		}

		tagvalue, err := entry.value(r, h.byteOrder)
		if err != nil {
			return nil, h, err
		}
		tags[entry.Tag] = *tagvalue
	}

	// We explicitly DO NOT read the offset to the next IFD and loop.
	return tags, h, nil
}

func (ifd *iFDEntry) value(r io.ReadSeeker, byteOrder binary.ByteOrder) (*tagData, error) {
	t := tagData{fType: ifd.FType, length: uint32(ifd.Count)}
	var reader io.Reader
	if len(ifd.ValueBytes) > 0 {
		reader = bytes.NewReader(ifd.ValueBytes)
	} else {
		readerAt, ok := r.(io.ReaderAt)
		if !ok {
			return nil, errors.New("reader does not implement io.ReaderAt")
		}
		reader = io.NewSectionReader(readerAt, int64(ifd.ValueOffset), int64(ifd.FType.bytes())*int64(ifd.Count))
	}
	switch ifd.FType {
	case BYTE:
		t.byteData = make([]uint8, ifd.Count)
		if err := binary.Read(reader, byteOrder, &t.byteData); err != nil {
			return nil, err
		}
	case ASCII:
		p := make([]uint8, ifd.Count)
		if err := binary.Read(reader, byteOrder, p); err != nil {
			return nil, err
		}
		t.asciiData = string(bytes.Trim(p, "\x00"))
	case SHORT:
		t.shortData = make([]uint16, ifd.Count)
		if err := binary.Read(reader, byteOrder, &t.shortData); err != nil {
			return nil, err
		}
	case LONG:
		t.longData = make([]uint32, ifd.Count)
		if err := binary.Read(reader, byteOrder, &t.longData); err != nil {
			return nil, err
		}
	case FLOAT:
		t.floatData = make([]float32, ifd.Count)
		if err := binary.Read(reader, byteOrder, t.floatData); err != nil {
			return nil, err
		}
	case DOUBLE:
		t.doubleData = make([]float64, ifd.Count)
		if err := binary.Read(reader, byteOrder, &t.doubleData); err != nil {
			return nil, err
		}
	case LONG8, IFD8:
		t.uint64Data = make([]uint64, ifd.Count)
		if err := binary.Read(reader, byteOrder, &t.uint64Data); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported type for value reading: %d", ifd.FType)
	}
	return &t, nil
}

func (g *GeoTIFF) loc(x, y int) (float32, error) {
	// Validate that the requested coordinates fall within the image bounds
	if x < 0 || x >= int(g.imageWidth) || y < 0 || y >= int(g.imageLength) {
		return 0.0, errors.New("point lies outside image")
	}

	// Calculate which tile contains the requested pixel
	tileX := x / int(g.tileWidth)  // Tile column index
	tileY := y / int(g.tileLength) // Tile row index
	tileNum := g.tilesAcross*tileY + tileX

	// Get the *processed* tile data. This will be a typed slice (e.g., []float32).
	// This is a blocking call for the primary tile.
	processedTile, err := g.getTileData(tileNum)
	if err != nil {
		return 0, fmt.Errorf("failed to get data for tile %d: %w", tileNum, err)
	}

	// After successfully getting the primary tile, trigger a non-blocking
	// prefetch for its neighbors. This improves performance for subsequent
	// requests that are spatially close.
	prefetchKey := fmt.Sprintf("prefetch-%d", tileNum)
	go g.inflightPrefetch.Do(prefetchKey, func() (interface{}, error) {
		g.prefetchNeighbors(tileNum)
		// We add a short "Forget" duration. This allows another
		// prefetch to be triggered for the same tile after a while,
		// which can be useful if cache items expire.
		time.AfterFunc(1*time.Minute, func() {
			g.inflightPrefetch.Forget(prefetchKey)
		})
		return nil, nil
	})

	// Calculate the pixel's linear index within its tile
	idI := x % int(g.tileWidth)
	idJ := y % int(g.tileLength)
	pixelIndexInTile := idJ*int(g.tileWidth) + idI

	// Type-assert the cached data and extract the value directly.
	// This is extremely fast as the expensive processing is already done.
	switch data := processedTile.(type) {
	case []float32:
		if pixelIndexInTile >= len(data) {
			return 0, fmt.Errorf("pixel index %d out of tile bounds (%d)", pixelIndexInTile, len(data))
		}
		return data[pixelIndexInTile], nil

	case []int32:
		if pixelIndexInTile >= len(data) {
			return 0, fmt.Errorf("pixel index %d out of tile bounds (%d)", pixelIndexInTile, len(data))
		}
		rawValue := data[pixelIndexInTile]
		// Apply 0.1 scaling factor for integer data
		return float32(rawValue) * 0.1, nil

	default:
		return 0, fmt.Errorf("unexpected data type in cache: %T", processedTile)
	}
}

// getTileData retrieves a tile, processes it into a typed slice, and caches the result.
func (g *GeoTIFF) getTileData(tileNum int) (any, error) {
	key := strconv.Itoa(tileNum)
	// Check for a processed tile in the cache.
	item := g.tileCache.Get(key)
	if item != nil && !item.Expired() {
		return item.Value(), nil
	}

	// If not in cache, use singleflight to ensure only one goroutine
	// fetches and processes the tile.
	v, err, _ := g.inflightData.Do(key, func() (interface{}, error) {
		// 1. Fetch and decompress raw bytes.
		decompressedBytes, fetchErr := g.fetchAndDecompressTile(tileNum)
		if fetchErr != nil {
			return nil, fetchErr
		}

		// 2. Process the raw bytes into a typed slice. This happens only ONCE.
		var processedData any
		var processingErr error

		switch g.sampleFormat {
		case SampleFormatFloat:
			if g.bitsPerSample != 32 {
				return nil, fmt.Errorf("unsupported bit depth for float: %d", g.bitsPerSample)
			}
			numPixelsInTile := len(decompressedBytes) / 4
			tileData := make([]float32, numPixelsInTile)
			if err := binary.Read(bytes.NewReader(decompressedBytes), g.byteOrder, &tileData); err != nil {
				processingErr = err
			} else {
				processedData = tileData
			}
		case SampleFormatInt:
			if g.bitsPerSample != 32 {
				return nil, fmt.Errorf("unsupported bit depth for int: %d", g.bitsPerSample)
			}
			numPixelsInTile := len(decompressedBytes) / 4
			tileData := make([]int32, numPixelsInTile)
			if err := binary.Read(bytes.NewReader(decompressedBytes), g.byteOrder, &tileData); err != nil {
				processingErr = err
			} else {
				if g.predictor == PredictorHorizontal {
					undoHorizontalPredictionForInt32(tileData, g.tileWidth, g.tileLength)
				}
				processedData = tileData
			}
		default:
			processingErr = fmt.Errorf("unsupported sample format (SampleFormat: %d, BitsPerSample: %d)", g.sampleFormat, g.bitsPerSample)
		}

		if processingErr != nil {
			return nil, processingErr
		}

		// 3. Cache the final, processed, typed slice.
		g.tileCache.Set(key, processedData, 10*time.Minute)
		return processedData, nil
	})

	if err != nil {
		return nil, err
	}
	return v, nil
}

// fetchAndDecompressTile performs the I/O to read and decompress a single tile.
// This function remains unchanged.
func (g *GeoTIFF) fetchAndDecompressTile(tileNum int) ([]byte, error) {
	if uint64(tileNum) >= uint64(len(g.tileOffsets)) {
		return nil, fmt.Errorf("tile index %d out of bounds", tileNum)
	}

	offset := g.tileOffsets[tileNum]
	byteCount := g.tileByteCounts[tileNum]
	tileBytes := make([]byte, byteCount)

	readerAt, ok := g.reader.(io.ReaderAt)
	if !ok {
		return nil, errors.New("reader does not support ReadAt for tile fetching")
	}
	if _, err := readerAt.ReadAt(tileBytes, int64(offset)); err != nil {
		return nil, fmt.Errorf("failed to read tile %d from source: %w", tileNum, err)
	}

	var decompressedBytes []byte
	switch g.compression {
	case Uncompressed:
		decompressedBytes = tileBytes
	case DEFLATE:
		z, err := zlib.NewReader(bytes.NewReader(tileBytes))
		if err != nil {
			return nil, fmt.Errorf("failed to create zlib reader for tile: %w", err)
		}
		defer z.Close()
		decompressedBytes, err = io.ReadAll(z)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress tile data: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported compression type: %d", g.compression)
	}
	return decompressedBytes, nil
}

// prefetchNeighbors is a "dumb" function that fetches neighbors but does not
// trigger any further prefetching.
func (g *GeoTIFF) prefetchNeighbors(tileNum int) {
	if g.tilesAcross == 0 {
		return
	}

	tileY := tileNum / g.tilesAcross
	tileX := tileNum % g.tilesAcross
	totalRows := int(g.imageLength+g.tileLength-1) / int(g.tileLength)

	var wg sync.WaitGroup
	for j := -1; j <= 1; j++ {
		for i := -1; i <= 1; i++ {
			if i == 0 && j == 0 {
				continue
			}

			neighborX := tileX + i
			neighborY := tileY + j

			if neighborX >= 0 && neighborX < g.tilesAcross && neighborY >= 0 && neighborY < totalRows {
				neighborTileNum := neighborY*g.tilesAcross + neighborX
				wg.Add(1)
				go func(num int) {
					defer wg.Done()
					// Call the getter to populate the cache; ignore return values for "fire-and-forget" prefetch.
					g.getTileData(num)
				}(neighborTileNum)
			}
		}
	}
	wg.Wait() // Wait for all neighbor fetches to complete.
}

func (g *GeoTIFF) getUint(tag Tag) (uint64, bool) {
	t, ok := g.tags[tag]
	if !ok {
		return 0, false
	}
	if t.fType == SHORT && len(t.shortData) > 0 {
		return uint64(t.shortData[0]), true
	}
	if t.fType == LONG && len(t.longData) > 0 {
		return uint64(t.longData[0]), true
	}
	return 0, false
}

func (g *GeoTIFF) getShort(tag Tag) (uint16, bool) {
	t, ok := g.tags[tag]
	if !ok {
		return 0, false
	}
	if t.fType == SHORT && len(t.shortData) > 0 {
		return t.shortData[0], true
	}
	return 0, false
}

func (g *GeoTIFF) get64bitSlice(tag Tag) ([]uint64, bool) {
	t, ok := g.tags[tag]
	if !ok {
		return nil, false
	}
	if t.fType == LONG8 || t.fType == IFD8 {
		return t.uint64Data, true
	}
	if t.fType == LONG {
		res := make([]uint64, len(t.longData))
		for i, v := range t.longData {
			res[i] = uint64(v)
		}
		return res, true
	}
	return nil, false
}

func (td tagData) doubleDataValue() ([]float64, bool) {
	if td.fType == DOUBLE {
		return td.doubleData, true
	}
	return nil, false
}

func (p Point) String() string { return fmt.Sprintf("(Lon: %f, Lat: %f)", p.Lon, p.Lat) }

func (cc *CornerCoordinates) String() string {
	return fmt.Sprintf("UL: %s, LR: %s", cc.UpperLeft.String(), cc.LowerRight.String())
}
func (cc *CornerCoordinates) Contains(p Point) bool {
	minLon := math.Min(cc.UpperLeft.Lon, cc.LowerRight.Lon)
	maxLon := math.Max(cc.UpperLeft.Lon, cc.LowerRight.Lon)
	minLat := math.Min(cc.UpperLeft.Lat, cc.LowerRight.Lat)
	maxLat := math.Max(cc.UpperLeft.Lat, cc.LowerRight.Lat)
	return p.Lon >= minLon && p.Lon <= maxLon && p.Lat >= minLat && p.Lat <= maxLat
}

// undoHorizontalPredictionForInt32 reverses the horizontal differencing predictor.
// It must be called on the int32 slice after decompression.
func undoHorizontalPredictionForInt32(data []int32, tileWidth, tileHeight uint32) {
	if tileWidth == 0 || tileHeight == 0 {
		return
	}
	for y := 0; y < int(tileHeight); y++ {
		rowStart := y * int(tileWidth)
		if rowStart+int(tileWidth) > len(data) {
			break
		}
		for x := 1; x < int(tileWidth); x++ {
			index := rowStart + x
			prevIndex := index - 1
			data[index] += data[prevIndex]
		}
	}
}
