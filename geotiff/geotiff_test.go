// geotiff/geotiff_test.go

package geotiff

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"testing"
)

// floatEquals compares two float32 values with a small tolerance (epsilon).
func floatEquals(a, b float32) bool {
	const epsilon = 1e-4 // A small tolerance for float comparison
	return math.Abs(float64(a-b)) < epsilon
}

func TestAtCoord(t *testing.T) {
	// Open the test COG file from the testdata directory.
	// The path is relative to the test file's location.
	f, err := os.Open("testdata/extend-cog.tiff")
	if err != nil {
		t.Fatalf("failed to open test file: %v", err)
	}
	defer f.Close()

	// Initialize the GeoTIFF reader with our test file.
	geo, err := Open(f, 5, 1)
	if err != nil {
		t.Fatalf("failed to open GeoTIFF: %v", err)
	}

	// Define test cases in a table-driven format.
	testCases := []struct {
		name          string
		lon           float64
		lat           float64
		wantElevation float32
		wantErr       bool
		errContains   string // Substring to check for in the error message
	}{
		{
			name:          "Valid point inside bounds (Mont Blanc)",
			lon:           6.86487244,
			lat:           45.83291118,
			wantElevation: 4805.3,
			wantErr:       false,
		}, {
			name:          "Valid point used in profile test",
			lon:           6.87557563,
			lat:           45.84747045,
			wantElevation: 4379.4,
			wantErr:       false,
		},
		{
			name:        "Invalid point outside bounds (0,0)",
			lon:         0.0,
			lat:         0.0,
			wantErr:     true,
			errContains: "does not fall inside the image bounds",
		},
		{
			name:        "Invalid point at top edge",
			lon:         6.8,
			lat:         50.0, // Clearly above the max latitude
			wantErr:     true,
			errContains: "does not fall inside the image bounds",
		},
		{
			name:        "Invalid point at left edge",
			lon:         5.0, // Clearly left of the min longitude
			lat:         45.8,
			wantErr:     true,
			errContains: "does not fall inside the image bounds",
		},
	}

	for _, tc := range testCases {
		// t.Run creates a sub-test, which gives clearer output on failure.
		t.Run(tc.name, func(t *testing.T) {
			// Call the function we want to test.
			gotElevation, err := geo.AtCoord(tc.lon, tc.lat)

			// Check if we got an error when we expected one.
			if tc.wantErr {
				if err == nil {
					t.Errorf("AtCoord(%f, %f) expected an error, but got none", tc.lon, tc.lat)
					return // Stop this sub-test
				}
				// Check if the error message contains the expected text.
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("AtCoord(%f, %f) error message\n got: %q\nwant to contain: %q", tc.lon, tc.lat, err.Error(), tc.errContains)
				}
				return // Test case passed, continue to the next one
			}

			// Check if we got an unexpected error.
			if err != nil {
				t.Errorf("AtCoord(%f, %f) returned an unexpected error: %v", tc.lon, tc.lat, err)
				return
			}

			// Compare the returned elevation with the expected value using our tolerant comparison.
			if !floatEquals(gotElevation, tc.wantElevation) {
				t.Errorf("AtCoord(%f, %f)\n got elevation: %f\nwant elevation: %f", tc.lon, tc.lat, gotElevation, tc.wantElevation)
			}
		})
	}
}

func TestBounds(t *testing.T) {
	f, err := os.Open("testdata/extend-cog.tiff")
	if err != nil {
		t.Fatalf("failed to open test file: %v", err)
	}
	defer f.Close()

	geo, err := Open(f, 5, 1)
	if err != nil {
		t.Fatalf("failed to open GeoTIFF: %v", err)
	}

	bounds, err := geo.Bounds()
	if err != nil {
		t.Fatalf("Bounds() returned an unexpected error: %v", err)
	}

	// Example expected bounds string from your error message.
	// UL: (Lon: 6.779250, Lat: 45.926250), LR: (Lon: 6.964000, Lat: 45.727500)
	expectedString := "UL: (Lon: 6.779250, Lat: 45.926250), LR: (Lon: 6.964000, Lat: 45.727500)"
	gotString := fmt.Sprintf("UL: (Lon: %f, Lat: %f), LR: (Lon: %f, Lat: %f)",
		bounds.UpperLeft.Lon, bounds.UpperLeft.Lat, bounds.LowerRight.Lon, bounds.LowerRight.Lat)

	// Note: Direct string comparison of floats can be tricky. This is more of a sanity check.
	// For a real test, you'd compare the float fields with a tolerance.
	if gotString != expectedString {
		t.Logf("Note: Float formatting may cause differences.")
		t.Logf("Got:      %s", gotString)
		t.Logf("Expected: %s", expectedString)
	}

	// A better comparison:
	if !floatEquals(float32(bounds.UpperLeft.Lon), 6.779250) ||
		!floatEquals(float32(bounds.UpperLeft.Lat), 45.926250) ||
		!floatEquals(float32(bounds.LowerRight.Lon), 6.964000) ||
		!floatEquals(float32(bounds.LowerRight.Lat), 45.727500) {
		t.Errorf("Bounds() returned incorrect values. Got %+v", bounds)
	}
}

func TestProfile(t *testing.T) {
	// Open the test COG file from the testdata directory.
	f, err := os.Open("testdata/extend-cog.tiff")
	if err != nil {
		t.Fatalf("failed to open test file: %v", err)
	}
	defer f.Close()

	// Initialize the GeoTIFF reader with our test file.
	geo, err := Open(f, 5, 1)
	if err != nil {
		t.Fatalf("failed to open GeoTIFF: %v", err)
	}

	// Define the start and end points for the profile request.
	// Note: The input format is [lat, lng].
	requestCoordinates := [][]float64{
		{45.84747045, 6.87557563}, // Start point
		{45.84743098, 6.87606897}, // End point
	}

	// Call the Profile function.
	profile, err := geo.Profile(requestCoordinates)
	if err != nil {
		t.Fatalf("Profile() returned an unexpected error: %v", err)
	}

	// Check if the profile has the expected number of points.
	// As per the request, we expect exactly 3 points for this short segment.
	expectedPointCount := 3
	if len(profile) != expectedPointCount {
		t.Errorf("expected %d points in profile, but got %d", expectedPointCount, len(profile))
		// Log the points we actually got for debugging.
		for i, p := range profile {
			t.Logf("Point %d: [Lat: %f, Lon: %f, Elv: %f]", i, p[0], p[1], p[2])
		}
		t.FailNow() // Stop the test here if the point count is wrong.
	}

	// Check the elevation of the first point.
	expectedStartElevation := float32(4379.4)
	if !floatEquals(float32(profile[0][2]), expectedStartElevation) {
		t.Errorf("start point elevation mismatch: got %f, want %f", profile[0][2], expectedStartElevation)
	}

	// Check the elevation of the middle point.
	expectedMiddleElevation := float32(4371.4)
	if !floatEquals(float32(profile[1][2]), expectedMiddleElevation) {
		t.Errorf("middle point elevation mismatch: got %f, want %f", profile[1][2], expectedMiddleElevation)
	}

	// Check the elevation of the last point.
	expectedEndElevation := float32(4361.7)
	if !floatEquals(float32(profile[2][2]), expectedEndElevation) {
		t.Errorf("end point elevation mismatch: got %f, want %f", profile[2][2], expectedEndElevation)
	}

	// Optional: Log the results on success for visibility.
	t.Log("Profile test passed. Points found:")
	for i, p := range profile {
		t.Logf("  Point %d: [Lat: %.8f, Lon: %.8f, Elv: %.1f]", i, p[0], p[1], p[2])
	}
}

func TestCacheCompression(t *testing.T) {
	// Open the test COG file from the testdata directory.
	f, err := os.Open("testdata/extend-cog.tiff")
	if err != nil {
		t.Fatalf("failed to open test file: %v", err)
	}
	defer f.Close()

	// Initialize the GeoTIFF reader with a reasonable cache size.
	geo, err := Open(f, 1024*1024*100, 100) // 100MB cache
	if err != nil {
		t.Fatalf("failed to open GeoTIFF: %v", err)
	}

	// First, access some coordinates to populate the cache.
	// Mont Blanc area - this should load at least one tile.
	testCoords := []struct {
		lon float64
		lat float64
	}{
		{6.86487244, 45.83291118}, // Mont Blanc
		{6.87557563, 45.84747045}, // Point from profile test
		{6.90000000, 45.85000000}, // Another nearby point
	}

	// Access coordinates to populate cache.
	for _, coord := range testCoords {
		_, err := geo.AtCoord(coord.lon, coord.lat)
		if err != nil {
			t.Logf("AtCoord(%f, %f) returned error (may be out of bounds): %v", coord.lon, coord.lat, err)
		}
	}

	// Analyze cache contents.
	var totalUncompressedSize int
	var tileCount int

	// Iterate through all possible tile numbers to find cached tiles.
	for i := 0; i < len(geo.tileOffsets); i++ {
		key := fmt.Sprintf("%d", i)
		item := geo.tileCache.Get(key)
		if item != nil && !item.Expired() {
			tileCount++

			// Calculate memory size based on sample format.
			// Tile dimensions are tileWidth x tileLength pixels.
			pixelsPerTile := int(geo.tileWidth * geo.tileLength)
			bytesPerPixel := geo.bitsPerSample / 8
			tileSize := pixelsPerTile * int(bytesPerPixel)
			totalUncompressedSize += tileSize

			t.Logf("Tile %d: size=%d bytes (%d pixels)", i, tileSize, pixelsPerTile)
		}
	}

	if tileCount == 0 {
		t.Log("No tiles were cached. This may indicate coordinates are outside the test file bounds.")
		t.Skip("Skipping cache test - no tiles in cache")
	}

	// Report overall statistics.
	t.Logf("\n=== Cache Statistics ===")
	t.Logf("Total tiles cached: %d", tileCount)
	t.Logf("Total cache memory: %d bytes (%.2f KB)", totalUncompressedSize, float64(totalUncompressedSize)/1024)
	t.Logf("Average tile size: %d bytes", totalUncompressedSize/tileCount)

	// Test that we can read from cache and get correct values.
	// Access the same coordinates again - they should be served from cache.
	for _, coord := range testCoords {
		val1, err := geo.AtCoord(coord.lon, coord.lat)
		if err != nil {
			continue // Skip if out of bounds
		}

		// Access again to verify cache hit returns same value.
		val2, err := geo.AtCoord(coord.lon, coord.lat)
		if err != nil {
			t.Errorf("Second AtCoord(%f, %f) returned error: %v", coord.lon, coord.lat, err)
			continue
		}

		if !floatEquals(val1, val2) {
			t.Errorf("Cache returned different values for same coordinate: first=%f, second=%f", val1, val2)
		}
	}

	t.Log("Cache test passed - data integrity verified")
}

func openTestVRT(t *testing.T) GeoRaster {
	t.Helper()

	f, err := os.Open("testdata/extend-cog.vrt")
	if err != nil {
		t.Fatalf("failed to open test VRT file: %v", err)
	}
	defer f.Close()

	factory := func(ctx context.Context, filename string) (io.ReadSeeker, error) {
		return os.Open("testdata/" + filename)
	}

	geo, err := OpenVRT(f, factory, 5, 1, 0)
	if err != nil {
		t.Fatalf("failed to open VRT: %v", err)
	}
	return geo
}

func TestVRTAtCoord(t *testing.T) {
	geo := openTestVRT(t)

	testCases := []struct {
		name          string
		lon           float64
		lat           float64
		wantElevation float32
		wantErr       bool
		errContains   string
	}{
		{
			name:          "Valid point inside bounds (Mont Blanc)",
			lon:           6.86487244,
			lat:           45.83291118,
			wantElevation: 4805.3,
			wantErr:       false,
		},
		{
			name:          "Valid point used in profile test",
			lon:           6.87557563,
			lat:           45.84747045,
			wantElevation: 4379.4,
			wantErr:       false,
		},
		{
			name:        "Invalid point outside bounds (0,0)",
			lon:         0.0,
			lat:         0.0,
			wantErr:     true,
			errContains: "does not fall inside the image bounds",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gotElevation, err := geo.AtCoord(tc.lon, tc.lat)

			if tc.wantErr {
				if err == nil {
					t.Errorf("AtCoord(%f, %f) expected an error, but got none", tc.lon, tc.lat)
					return
				}
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("AtCoord(%f, %f) error message\n got: %q\nwant to contain: %q", tc.lon, tc.lat, err.Error(), tc.errContains)
				}
				return
			}

			if err != nil {
				t.Errorf("AtCoord(%f, %f) returned an unexpected error: %v", tc.lon, tc.lat, err)
				return
			}

			if !floatEquals(gotElevation, tc.wantElevation) {
				t.Errorf("AtCoord(%f, %f)\n got elevation: %f\nwant elevation: %f", tc.lon, tc.lat, gotElevation, tc.wantElevation)
			}
		})
	}
}

func TestVRTBounds(t *testing.T) {
	geo := openTestVRT(t)

	bounds, err := geo.Bounds()
	if err != nil {
		t.Fatalf("Bounds() returned an unexpected error: %v", err)
	}

	if !floatEquals(float32(bounds.UpperLeft.Lon), 6.779250) ||
		!floatEquals(float32(bounds.UpperLeft.Lat), 45.926250) ||
		!floatEquals(float32(bounds.LowerRight.Lon), 6.964000) ||
		!floatEquals(float32(bounds.LowerRight.Lat), 45.727500) {
		t.Errorf("Bounds() returned incorrect values. Got %+v", bounds)
	}
}

func TestVRTProfile(t *testing.T) {
	geo := openTestVRT(t)

	requestCoordinates := [][]float64{
		{45.84747045, 6.87557563},
		{45.84743098, 6.87606897},
	}

	profile, err := geo.Profile(requestCoordinates)
	if err != nil {
		t.Fatalf("Profile() returned an unexpected error: %v", err)
	}

	expectedPointCount := 3
	if len(profile) != expectedPointCount {
		t.Errorf("expected %d points in profile, but got %d", expectedPointCount, len(profile))
		for i, p := range profile {
			t.Logf("Point %d: [Lat: %f, Lon: %f, Elv: %f]", i, p[0], p[1], p[2])
		}
		t.FailNow()
	}

	expectedStartElevation := float32(4379.4)
	if !floatEquals(float32(profile[0][2]), expectedStartElevation) {
		t.Errorf("start point elevation mismatch: got %f, want %f", profile[0][2], expectedStartElevation)
	}

	expectedMiddleElevation := float32(4371.4)
	if !floatEquals(float32(profile[1][2]), expectedMiddleElevation) {
		t.Errorf("middle point elevation mismatch: got %f, want %f", profile[1][2], expectedMiddleElevation)
	}

	expectedEndElevation := float32(4361.7)
	if !floatEquals(float32(profile[2][2]), expectedEndElevation) {
		t.Errorf("end point elevation mismatch: got %f, want %f", profile[2][2], expectedEndElevation)
	}
}

func TestLZWCog(t *testing.T) {
	f, err := os.Open("testdata/lzw-cog.tiff")
	if err != nil {
		t.Fatalf("failed to open LZW test file: %v", err)
	}
	defer f.Close()

	geo, err := Open(f, 5, 1)
	if err != nil {
		t.Fatalf("failed to open LZW GeoTIFF: %v", err)
	}

	bounds, err := geo.Bounds()
	if err != nil {
		t.Fatalf("Bounds() error: %v", err)
	}
	t.Logf("Bounds: %s", bounds.String())

	// Test known elevation values from GDAL reference
	tests := []struct {
		name     string
		lon      float64
		lat      float64
		expected float32
		tolerance float32
	}{
		{
			name:     "top-left corner (slightly inset)",
			lon:      -121.953148 + 0.000010,
			lat:      37.194630 - 0.000010,
			expected: 474.2,
			tolerance: 0.5,
		},
		{
			name:     "center pixel",
			lon:      -121.929444,
			lat:      37.170926,
			expected: 845.2,
			tolerance: 0.5,
		},
		{
			name:     "known pixel (462,173)",
			lon:      -121.910370,
			lat:      37.178611,
			expected: 915.9,
			tolerance: 0.5,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			val, err := geo.AtCoord(tc.lon, tc.lat)
			if err != nil {
				t.Errorf("AtCoord(%f, %f) error: %v", tc.lon, tc.lat, err)
				return
			}
			diff := val - tc.expected
			if diff < 0 {
				diff = -diff
			}
			if diff > tc.tolerance {
				t.Errorf("AtCoord(%f, %f) = %f, want %f (±%f)", tc.lon, tc.lat, val, tc.expected, tc.tolerance)
			} else {
				t.Logf("AtCoord(%f, %f) = %f (expected %f)", tc.lon, tc.lat, val, tc.expected)
			}
		})
	}

	// Verify bounds
	if !floatEquals(float32(bounds.UpperLeft.Lon), -121.953148) ||
		!floatEquals(float32(bounds.UpperLeft.Lat), 37.194630) {
		t.Errorf("Bounds UL mismatch: got (%f, %f)", bounds.UpperLeft.Lon, bounds.UpperLeft.Lat)
	}
}

func TestLZWRemote(t *testing.T) {
	data, err := os.ReadFile("testdata/lzw-cog.tiff")
	if err != nil {
		t.Fatalf("failed to read LZW test file: %v", err)
	}

	reader := bytes.NewReader(data)
	tif, id, err := openRemoteTIFF(reader, int64(len(data)))
	if err != nil {
		t.Fatalf("openRemoteTIFF failed: %v", err)
	}
	defer func() {
		closeRemoteTIFF(tif)
		unregisterRemoteReader(id)
	}()

	raw, err := readTileFromHandle(tif, 0)
	if err != nil {
		t.Fatalf("readTileFromHandle failed: %v", err)
	}

	if len(raw) != 512*512*4 {
		t.Fatalf("unexpected tile size: %d, want %d", len(raw), 512*512*4)
	}

	floats := make([]float32, len(raw)/4)
	if err := binary.Read(bytes.NewReader(raw), binary.LittleEndian, &floats); err != nil {
		t.Fatalf("binary.Read failed: %v", err)
	}

	// Compare with GDAL reference values
	tests := []struct {
		pixelIdx int
		expected float32
		tolerance float32
	}{
		{0, 474.2, 0.5},
		{1, 474.9, 0.5},
		{173*512 + 462, 915.9, 0.5},
	}

	for _, tc := range tests {
		got := floats[tc.pixelIdx]
		diff := got - tc.expected
		if diff < 0 { diff = -diff }
		if diff > tc.tolerance {
			t.Errorf("pixel %d: got %f, want %f (±%f)", tc.pixelIdx, got, tc.expected, tc.tolerance)
		}
	}

	// Verify bounds by computing min/max
	var minVal, maxVal float32 = floats[0], floats[0]
	for _, v := range floats {
		if v < minVal { minVal = v }
		if v > maxVal { maxVal = v }
	}
	t.Logf("Tile range: min=%.1f max=%.1f", minVal, maxVal)
	if minVal < 200 || maxVal > 1200 {
		t.Errorf("unexpected elevation range: %.1f to %.1f", minVal, maxVal)
	}
}
