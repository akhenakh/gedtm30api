// geotiff/geotiff_test.go

package geotiff

import (
	"fmt"
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
	geo, err := Open(f)
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

	geo, err := Open(f)
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
	geo, err := Open(f)
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
