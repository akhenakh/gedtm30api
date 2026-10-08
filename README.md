# GEDTM30API

This project provides a high-performance elevation service from Cloud-Optimized GeoTIFF (COG) or GDAL VRT files.
It exposes the service via both a traditional HTTP REST API and a modern, high-performance gRPC API.
The service is optimized for low-latency queries by using on-demand tile fetching, in-memory caching, and concurrent request handling.

The primary data source is [Global Ensemble Digital Terrain Model 30m (GEDTM30)](https://zenodo.org/records/15689805).
It also supports [USGS 3DEP 1/3 arc-second (~10m)](https://prd-tnm.s3.amazonaws.com/index.html?prefix=StagedProducts/Elevation/) data via remote VRT files.

## Features

-   **Dual API Support**: Access elevation data through both a simple HTTP REST API and a typed, high-performance gRPC API.
-   **Cloud-Optimized GeoTIFF (COG) Support**: Efficiently reads data from large GeoTIFF files without needing to download the entire file. It fetches only the required header information and raster tiles on-demand.
-   **Flexible Data Sources**: Works seamlessly with GCS, S3, Azure Blob Storage, HTTP remote COGs (via HTTP range requests) or local `.tiff` files.
-   **VRT (Virtual Raster) Support**: Supports GDAL `.vrt` files for mosaicking multiple GeoTIFFs into a single virtual dataset. The VRT file is fetched from cloud/local just like a COG, and source tiles are lazily opened and cached on-demand. Remote VRT files referencing `/vsicurl/` paths are resolved transparently.
-   **LZW Compression Support**: Handles LZW-compressed TIFF tiles (common in USGS 3DEP data) via CGo/libgeotiff integration. Both local and remote LZW tiles are decoded using libtiff's proven decoder with predictor support.
-   **High-Performance Caching**: Utilizes an in-memory LRU cache (`ccache`) to store *processed* tile data. This dramatically reduces I/O and CPU load for subsequent requests to nearby geographic areas.
-   **Concurrent Request Handling**: Implements a `singleflight` mechanism to prevent the "thundering herd" problem, ensuring that concurrent requests for the same uncached tile result in only one fetch operation.
-   **Intelligent Prefetching** (opt-in): When enabled via `PREFETCH_NEIGHBORS`, a requested tile's neighbors are preemptively fetched in the background to improve latency for spatially coherent queries (like generating a profile). Off by default to avoid wasted I/O on sparse point lookups.
-   **Production-Ready Structure**: Includes separate servers for the API, health checks (`gRPC`), and metrics (`Prometheus`), all managed with graceful shutdown.

## Getting Started

### Data
The server is defaulting to the most recent url for GEDTM30, but you probably want to download the file and host it yourself (310GB for the whole world), since no guarantee this url will always be available.

You can also extract some extend, note that you will need to rebuild the file as follow:
```sh
gdal_translate -of COG \
  -co COMPRESS=DEFLATE \
  -co ZLEVEL=6 \
  -co TILED=YES \
  extend.tiff extend-cog.tiff
```

#### VRT Files (Virtual Raster)

VRT files allow you to combine multiple GeoTIFF files into a single virtual mosaic without duplicating data. To create a VRT from one or more GeoTIFF files:

```sh
gdalbuildvrt mosaic.vrt tile1.tiff tile2.tiff tile3.tiff
```

When the server detects a `.vrt` extension, it parses the VRT XML to understand the overall raster layout and lazily opens the referenced source TIFFs as queries come in. Open source readers are kept in an LRU bounded by `CACHE_MAX_OPEN_SOURCES`, and all sources share a single tile cache so that `CACHE_MAX_SIZE` bounds the *total* number of cached tiles across the whole mosaic (see [Cache Management](#cache-management)).

The VRT file itself can be stored in cloud storage alongside its source TIFFs — `relativeToVRT` paths in the VRT are resolved automatically against the VRT's directory. Remote VRT files referencing `/vsicurl/` paths (GDAL virtual filesystem) are handled transparently.

#### USGS 3DEP 1/3 arc-second (~10m) via Remote VRT

The USGS publishes elevation data as Cloud-Optimized GeoTIFFs on S3. You can serve the entire continental US at ~10m resolution by pointing at a remote VRT file — no data download required:

```sh
# Serve USGS 3DEP 1/3 arc-second (~10m) for tile 13 (California/Nevada area)
CGO_ENABLED=1 go build .
LOG_LEVEL=INFO COG_SOURCE="https://prd-tnm.s3.amazonaws.com/StagedProducts/Elevation/13/TIFF/USGS_Seamless_DEM_13.vrt" ./gedtm30api
```

Find other VRT tiles at: https://prd-tnm.s3.amazonaws.com/index.html?prefix=StagedProducts/Elevation/

**Note**: USGS tiles use LZW compression, which requires building with CGo enabled (`CGO_ENABLED=1`) and libtiff installed (`libtiff-devel` or `libtiff-dev`). The service falls back to Go's LZW decoder for non-CGo builds, but libtiff is recommended for correctness.

Example VRT structure:
```xml
<VRTDataset rasterXSize="739" rasterYSize="795">
    <GeoTransform>6.779250, 0.000250, 0.0, 45.926250, 0.0, -0.000250</GeoTransform>
    <VRTRasterBand dataType="Float32" band="1">
        <SimpleSource>
            <SourceFilename relativeToVRT="1">tile.tiff</SourceFilename>
            <SourceBand>1</SourceBand>
            <SrcRect xOff="0" yOff="0" xSize="739" ySize="795"/>
            <DstRect xOff="0" yOff="0" xSize="739" ySize="795"/>
        </SimpleSource>
    </VRTRasterBand>
</VRTDataset>
```

### Running the Server

```sh
go install github.com/akhenakh/gedtm30api@latest
gedtm30api
```

### Environment Variables
- LOG_LEVEL envDefault:"INFO"
- HTTP_PORT envDefault:"8080"
- API_PORT envDefault:"9200"
- HEALTH_PORT envDefault:"6666"
- METRICS_PORT envDefault:"8888"
- BUCKET_URI
  Example S3: BUCKET_URI="s3://my-bucket?region=us-east-1", OBJECT_KEY="path/to/image.tif"
  Example GCS: BUCKET_URI="gs://my-bucket", OBJECT_KEY="image.tif"
  Example Azure: BUCKET_URI="azblob://bucket" OBJECT_KEY="image.tif"
  Example File: BUCKET_URI="file:///path/to/dir", OBJECT_KEY="image.tif"
- COG_SOURCE envDefault:"https://s3.opengeohub.org/global/dtm/v1.2/gedtm_rf_m_30m_s_20060101_20151231_go_epsg.4326.3855_v1.2.tif"
  Use it for HTTP access. Can point to a `.tiff` or `.vrt` file.
- CACHE_MAX_SIZE envDefault:"1073741824" (1 GiB) decoded-tile cache budget in **bytes**. For a VRT this is the total budget shared across all source files. Sizing by bytes (not tile count) keeps the cap meaningful across sources with different tile sizes: a decoded tile is `tileWidth × tileHeight × 4` bytes, e.g. **16 MiB** for the GEDTM30 2048×2048 Float32 COG.
- CACHE_PRUNE_PERCENT envDefault:"10" percentage of the cache (by size) evicted in one pass once the byte budget is exceeded.
- CACHE_MAX_OPEN_SOURCES envDefault:"256" (VRT only) maximum number of source GeoTIFF handles kept open at once. Tile memory is bounded by `CACHE_MAX_SIZE`; this caps per-source metadata and libtiff handles/file descriptors. Ignored for a single COG.
- PREFETCH_NEIGHBORS envDefault:"false" when true, each query fetches the 8 tiles surrounding the requested tile in the background. This lowers latency for spatially coherent workloads (e.g. elevation profiles) but multiplies I/O per request, so leave it off for sparse single-point lookups.
- TILE_FETCH_CONCURRENCY envDefault:"4" number of concurrent HTTP range requests used to split up a single read once it exceeds `TILE_FETCH_CHUNK_SIZE`. A single TCP stream to object storage rarely saturates a high-latency link, so splitting a large fetch into several parallel ranges can aggregate throughput. Set to 1 to disable. Tiles are fetched whole because LZW cannot be partially decoded. Note: this only helps when per-connection throughput (not total link bandwidth) is the bottleneck; on a saturated WAN link it has no effect.
- TILE_FETCH_CHUNK_SIZE envDefault:"2097152" (2 MiB) read size above which a tile/header fetch is split into `TILE_FETCH_CONCURRENCY` concurrent range requests; reads at or below this size go out as a single request. Inside one AWS region, per-request latency (time-to-first-byte) typically dominates over per-connection throughput for a single COG tile (usually a few hundred KB to low single-digit MB), so splitting them is normally a net loss — the default keeps an ordinary tile fetch as one request and only parallelizes genuinely large reads.
- HEADER_PREFETCH_SIZE envDefault:"65536" bytes fetched in a single read when opening a source, so the tag parser reads the header from memory instead of one network request per tag. If a raster's IFD (including its `TileOffsets`/`TileByteCounts` arrays) extends past this prefix, the missing tag values are batch-fetched in one merged follow-up request rather than one request per missed tag — and for LZW sources that same fetch is reused by libtiff's own directory parse instead of being fetched a second time. Raise this if open logs show a `header prefetch too small for IFD` warning, to avoid that extra round trip entirely.
The server will start multiple services on different ports:
-   **HTTP REST & Web UI**: `http://localhost:8080`
-   **Prometheus Metrics**: `http://localhost:8888/metrics`
-   **gRPC API**: `localhost:9200`
-   **gRPC Health Checks**: `localhost:6666`

### Use a Local File

To use a local GeoTIFF or VRT file, set the `COG_SOURCE` environment variable to the file path:

```sh
COG_SOURCE="/path/to/your/data.tiff" gedtm30api

# Or for VRT files:
COG_SOURCE="/path/to/your/mosaic.vrt" gedtm30api
```

## API Reference

### HTTP REST API

The REST API is available on port `8080` by default.

#### Get Elevation for a Single Point

Returns the elevation in meters for a single latitude/longitude coordinate.

-   **Endpoint**: `/getElevation/{lat}/{lng}`
-   **Method**: `GET`
-   **URL Parameters**:
    -   `lat`: Latitude (float)
    -   `lng`: Longitude (float)

-   **Example Request**:
    ```sh
    curl http://localhost:8080/getElevation/45.8329/6.8648
    ```

-   **Example Success Response** (`200 OK`):
    ```json
    {
      "elevation": 4805.3,
      "latitude": 45.8329,
      "longitude": 6.8648
    }
    ```

#### Get Elevation Profile for a Path

Returns a list of elevation points along a path defined by two or more coordinates.

-   **Endpoint**: `/getProfile/`
-   **Method**: `POST`
-   **Request Body**: A JSON object containing a list of `[latitude, longitude]` pairs.

-   **Example Request**:
    ```sh
    curl -X POST -H "Content-Type: application/json" \
      -d '{"coordinates":[[46.5, 6.5], [46.6, 6.6]]}' \
      http://localhost:8080/getProfile/
    ```

-   **Example Success Response** (`200 OK`):
    A JSON array where each element is `[latitude, longitude, elevation]`.
    ```json
    [
      [46.5, 6.5, 435.6],
      [46.500135, 6.500150, 436.1],
      [46.500270, 6.500300, 436.8],
      ...
      [46.6, 6.6, 612.9]
    ]
    ```

### gRPC API

The service definition can be found in `proto/elevation.proto`.

#### Service: `ElevationService`

##### `GetElevation` RPC

Retrieves the elevation for a single coordinate.

-   **Request**: `ElevationRequest`
    ```protobuf
    message ElevationRequest {
      double latitude = 1;
      double longitude = 2;
    }
    ```
-   **Response**: `ElevationResponse`
    ```protobuf
    message ElevationResponse {
      float elevation = 1;
    }
    ```

-   **Example with `grpcurl`**:
    ```sh
    grpcurl -plaintext -d '{
      "latitude": 38.84019,
      "longitude": -79.605892
    }' localhost:9200 elevationapi.v1.ElevationService.GetElevation
    ```

##### `GetProfile` RPC

Retrieves an elevation profile along a path.

-   **Request**: `ProfileRequest`
    ```protobuf
    message ProfileRequest {
      repeated Point points = 1;
    }
    message Point {
      double latitude = 1;
      double longitude = 2;
    }
    ```
-   **Response**: `ProfileResponse`
    ```protobuf
    message ProfileResponse {
      repeated ElevationPoint points = 1;
    }
    message ElevationPoint {
      double latitude = 1;
      double longitude = 2;
      float elevation = 3;
    }
    ```

-   **Example with `grpcurl`**:
    ```sh
    grpcurl -plaintext -d '{"points": [{"latitude": 46.5, "longitude": 6.5}, {"latitude": 46.6, "longitude": 6.6}]}' \
      localhost:9200 elevationapi.ElevationService.GetProfile
    ```

## Web Interface

A simple web UI is available at `http://localhost:8080`. It allows you to interactively click on a map to get single-point elevations or generate and visualize elevation profiles.

![Web Interface Screenshot](./img/elevation.png)

## Cache Management

The server uses a cache to store recently accessed *decoded* tiles. `CACHE_MAX_SIZE` is the cache budget in **bytes** (default 1 GiB). When the budget is exceeded, the least recently used tiles are evicted down to `CACHE_MAX_SIZE × (1 − CACHE_PRUNE_PERCENT/100)`. Sizing by bytes rather than by tile count keeps the cap meaningful across sources with different tile geometry: a decoded tile costs `tileWidth × tileHeight × 4` bytes regardless of what is stored on disk — **16 MiB** for the GEDTM30 2048×2048 Float32 COG, 1 MiB for a 512×512 tile.

`CACHE_MAX_SIZE` is the total budget in both modes: a single COG's tiles share it, and a VRT shares one cache across all of its source files so the same byte limit bounds the whole mosaic.

### Sizing memory

Memory is roughly `CACHE_MAX_SIZE` for the decoded tiles plus Go runtime overhead. With the default 1 GiB budget, expect on the order of 1 GiB of cached tiles.

Mind the Go GC: with the default `GOGC=100` the heap is allowed to grow to about **twice** the live set before a collection, and the runtime does not promptly return freed pages to the OS, so RSS can plateau near `2 × CACHE_MAX_SIZE`. Set `GOMEMLIMIT` (for example to your container limit) to bound this, or lower `CACHE_MAX_SIZE`.

For a VRT, add a small bounded overhead for open source handles: `CACHE_MAX_OPEN_SOURCES` × (per-file metadata + one libtiff handle/FD), on the order of tens of MiB at the default of 256. Evicted source handles are released by the Go garbage collector once no in-flight read still references them.

When setting a container/Kubernetes memory limit, budget ~`2 × CACHE_MAX_SIZE` (or set `GOMEMLIMIT`) plus a few hundred MiB for the Go runtime and heap fragmentation. Note the per-tile TTL means the resident set is a ceiling, not a constant.

### Sizing CPU

This project targets Go 1.25+, so `GOMAXPROCS` is container-aware by default: it reads the cgroup CPU limit at startup (and periodically thereafter) instead of defaulting to the node's full core count. The resolved value is logged once at startup (`"runtime" "GOMAXPROCS"=...`).

That said, a Kubernetes/Linux CPU *limit* is a throughput cap (CPU-time per 100ms accounting period, enforced by the CFS bandwidth controller), not a parallelism cap, so `GOMAXPROCS` being correct doesn't make a very tight limit throttle-free. Work that only happens on a cache miss — TLS handshakes, S3 request signing, header/tag parsing, DEFLATE/LZW decompression — can burn more CPU time than a small limit allows within one period, pausing the process for the rest of it; a cache hit (a map lookup) never comes close to that budget. In practice this shows up as latency spikes specifically on the *first* query to a new area rather than general slowness. If you see that pattern, raise `resources.requests.cpu`/`resources.limits.cpu` rather than only tuning the cache.

## GeoTIFF library

The geoTIFF library supports DEFLATE-compressed and LZW-compressed COGs with horizontal and floating-point predictors.
It is reusing some code from [geotiff](https://github.com/gden173/geotiff).

LZW decompression is handled via CGo/libgeotiff (`libtiff-devel` required) for both local files and remote HTTP sources.
The library provides a `GeoRaster` interface that unifies direct GeoTIFF access and VRT-based virtual mosaics, allowing both to be used interchangeably throughout the codebase.

For builds without CGo (`CGO_ENABLED=0`), LZW support falls back to a pure Go decoder which may not handle all LZW variants correctly.
It was reported to work with USGS 10m data.
