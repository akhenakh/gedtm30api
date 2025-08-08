package geotiff

import (
	"errors"
	"fmt"
	"log"
	"math"
)

// coordToPixel converts geographic coordinates (lon, lat) to image pixel coordinates (x, y).
// It requires the pre-calculated image bounds.
func (g *GeoTIFF) coordToPixel(lon, lat float64, bounds *CornerCoordinates) (int, int, error) {
	if !bounds.Contains(Point{Lon: lon, Lat: lat}) {
		return 0, 0, fmt.Errorf("point (Lon: %f, Lat: %f) is outside the image bounds", lon, lat)
	}
	// Convert longitude to pixel index (X direction)
	x := int(math.Round(math.Abs(lon-bounds.UpperLeft.Lon) / g.PixelScaleX))
	// Convert latitude to pixel index (Y direction)
	y := int(math.Round(math.Abs(lat-bounds.UpperLeft.Lat) / math.Abs(g.PixelScaleY)))
	return x, y, nil
}

// pixelToCoord converts image pixel coordinates (x, y) back to geographic coordinates (lon, lat).
// It requires the pre-calculated image bounds.
func (g *GeoTIFF) pixelToCoord(x, y int, bounds *CornerCoordinates) (float64, float64) {
	// g.PixelScaleY is negative, so adding correctly calculates the latitude "down" from the upper left corner.
	lon := bounds.UpperLeft.Lon + (float64(x) * g.PixelScaleX)
	lat := bounds.UpperLeft.Lat + (float64(y) * g.PixelScaleY)
	return lon, lat
}

// Profile calculates the elevation profile along a path defined by a series of geographic coordinates.
// It samples the elevation at a resolution equivalent to the raster's native pixel grid.
func (g *GeoTIFF) Profile(coordinates [][]float64) ([][]float64, error) {
	if len(coordinates) < 2 {
		return nil, errors.New("at least two coordinate pairs are required to create a profile")
	}

	bounds, err := g.Bounds()
	if err != nil {
		return nil, fmt.Errorf("failed to get image bounds: %w", err)
	}

	var profile [][]float64
	visitedPixels := make(map[string]struct{})

	// Iterate over each segment of the path
	for i := 0; i < len(coordinates)-1; i++ {
		startCoords := coordinates[i]
		endCoords := coordinates[i+1]

		if len(startCoords) != 2 || len(endCoords) != 2 {
			return nil, fmt.Errorf("invalid coordinate pair at index %d; expected [lat, lng]", i)
		}

		startLat, startLon := startCoords[0], startCoords[1]
		endLat, endLon := endCoords[0], endCoords[1]

		x1, y1, err := g.coordToPixel(startLon, startLat, bounds)
		if err != nil {
			return nil, err
		}
		x2, y2, err := g.coordToPixel(endLon, endLat, bounds)
		if err != nil {
			return nil, err
		}

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

			elevation, err := g.loc(currX, currY)
			if err != nil {
				log.Printf("Warning: could not get elevation for pixel (%d, %d): %v", currX, currY, err)
				continue
			}

			lon, lat := g.pixelToCoord(currX, currY, bounds)

			profile = append(profile, []float64{lat, lon, float64(elevation)})
		}
	}

	return profile, nil
}
