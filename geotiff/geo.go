package geotiff

type GeoRaster interface {
	AtCoord(lon, lat float64) (float32, error)
	Bounds() (*CornerCoordinates, error)
	Profile(coordinates [][]float64) ([][]float64, error)
}
