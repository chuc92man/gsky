package gdalprocess

// #include "gdal.h"
// #include "gdal_alg.h"
// #include "ogr_api.h"
// #include "ogr_srs_api.h"
// #include "cpl_string.h"
// #cgo pkg-config: gdal
import "C"

import (
	"fmt"
	"log"
	"math"
	"sort"
	"syscall"
	"unsafe"

	"encoding/json"

	geo "github.com/nci/geometry"
	pb "github.com/nci/gsky/worker/gdalservice"
)

type DrillFileDescriptor struct {
	OffX, OffY     int32
	CountX, CountY int32
	Mask           []uint8
}

var cWGS84WKT = C.CString(`GEOGCS["WGS 84",DATUM["WGS_1984",SPHEROID["WGS 84",6378137,298.257223563,AUTHORITY["EPSG","7030"]],TOWGS84[0,0,0,0,0,0,0],AUTHORITY["EPSG","6326"]],PRIMEM["Greenwich",0,AUTHORITY["EPSG","8901"]],UNIT["degree",0.0174532925199433,AUTHORITY["EPSG","9108"]],AUTHORITY["EPSG","4326"]]","proj4":"+proj=longlat +ellps=WGS84 +towgs84=0,0,0,0,0,0,0 +no_defs `)

func DrillDataset(in *pb.GeoRPCGranule) *pb.Result {

	var feat geo.Feature
	err := json.Unmarshal([]byte(in.Geometry), &feat)
	if err != nil {
		msg := fmt.Sprintf("Problem unmarshalling geometry %v", in)
		log.Println(msg)
		return &pb.Result{Error: msg}
	}
	geomGeoJSON, err := json.Marshal(feat.Geometry)
	if err != nil {
		msg := fmt.Sprintf("Problem marshaling GeoJSON geometry: %v", err)
		log.Println(msg)
		return &pb.Result{Error: msg}
	}

	if len(in.VRT) > 0 {
		vrtMgr, err := NewVRTManager([]byte(in.VRT))
		if err != nil {
			msg := fmt.Sprintf("VRT Manager error: %v", err)
			log.Printf(msg)
			return &pb.Result{Error: msg}
		}
		in.Path = vrtMgr.DSFileName

		defer vrtMgr.Close()
	}

	cPath := C.CString(in.Path)
	defer C.free(unsafe.Pointer(cPath))
	ds := C.GDALOpen(cPath, C.GDAL_OF_READONLY)
	if ds == nil {
		msg := fmt.Sprintf("GDAL could not open dataset: %s", in.Path)
		log.Println(msg)
		return &pb.Result{Error: msg}
	}
	defer C.GDALClose(ds)

	cGeom := C.CString(string(geomGeoJSON))
	defer C.free(unsafe.Pointer(cGeom))
	geom := C.OGR_G_CreateGeometryFromJson(cGeom)
	if geom == nil {
		msg := fmt.Sprintf("Geometry %s could not be parsed", in.Geometry)
		log.Println(msg)
		return &pb.Result{Error: msg}
	}

	selSRS := C.OSRNewSpatialReference(cWGS84WKT)
	defer C.OSRDestroySpatialReference(selSRS)

	C.OGR_G_AssignSpatialReference(geom, selSRS)

	res := readData(ds, in.Bands, geom, int(in.BandStrides), int(in.DrillDecileCount), int(in.PixelCount), in.ClipUpper, in.ClipLower)
	C.OGR_G_DestroyGeometry(geom)
	return res
}

func readData(ds C.GDALDatasetH, bands []int32, geom C.OGRGeometryH, bandStrides int, decileCount int, pixelCount int, clipUpper float32, clipLower float32) *pb.Result {
	nCols := 1 + decileCount

	avgs := []*pb.TimeSeries{}

	dsDscr, err := getDrillFileDescriptor(ds, geom)
	if err != nil {
		return &pb.Result{Error: err.Error()}
	}

	// it is safe to assume all data bands have same data type and nodata value
	bandH := C.GDALGetRasterBand(ds, C.int(1))
	dType := C.GDALGetRasterDataType(bandH)

	dSize := C.GDALGetDataTypeSizeBytes(dType)
	if dSize == 0 {
		err := fmt.Errorf("GDAL data type not implemented")
		return &pb.Result{Error: err.Error()}
	}

	if bandStrides <= 0 {
		bandStrides = 1
	}

	nodata := float32(C.GDALGetRasterNoDataValue(bandH, nil))
	metrics := &pb.WorkerMetrics{}

	var resUsage0, resUsage1 syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &resUsage0)

	// If we have a lot of bands, one may want to seek an approximate algorithm
	// to speed up the computation especially the RasterIO operation.
	// The approximate algorithm implemented here is linear interpolation between
	// the points in between the range with size specified by bandStrides.
	// For example, if bandStrides is 3. We then proceed as follows:
	// 1) Load band 1 and compute average for band 1 (i.e. avg1)
	// 2) Load band 3 and compute average for band 3 (i.e. avg3)
	// 3) Linearly interpolate avg2 using avg1 and avg3
	for ibBgn := 0; ibBgn < len(bands); ibBgn += bandStrides {
		ibEnd := ibBgn + bandStrides
		if ibEnd > len(bands) {
			ibEnd = len(bands)
		}

		bandsRead := []int32{bands[ibBgn], bands[ibEnd-1]}
		if bandStrides == 1 {
			bandsRead = bandsRead[:1]
		}

		effectiveNBands := len(bandsRead)

		dataBuf := make([]float32, dsDscr.CountX*dsDscr.CountY*int32(effectiveNBands))
		C.GDALDatasetRasterIO(ds, C.GF_Read, C.int(dsDscr.OffX), C.int(dsDscr.OffY), C.int(dsDscr.CountX), C.int(dsDscr.CountY), unsafe.Pointer(&dataBuf[0]), C.int(dsDscr.CountX), C.int(dsDscr.CountY), C.GDT_Float32, C.int(effectiveNBands), (*C.int)(unsafe.Pointer(&bandsRead[0])), 0, 0, 0)
		metrics.BytesRead += int64(len(dataBuf)) * int64(dSize)

		boundAvgs := make([]*pb.TimeSeries, effectiveNBands*nCols)
		bandSize := int(dsDscr.CountX * dsDscr.CountY)
		for iBand := 0; iBand < effectiveNBands; iBand++ {
			bandOffset := iBand * bandSize

			sum := float32(0)
			total := int32(0)

			for i := 0; i < bandSize; i++ {
				if dsDscr.Mask[i] == 255 && dataBuf[i+bandOffset] != nodata {
					val := dataBuf[i+bandOffset]
					if pixelCount != 0 {
						total++
					}

					if val < clipLower || val > clipUpper {
						continue
					}
					if pixelCount == 0 {
						sum += val
						total++
					} else {
						sum += 1.0
					}
				}
			}

			iRes := iBand * nCols
			if total > 0 {
				boundAvgs[iRes] = &pb.TimeSeries{Value: float64(sum / float32(total)), Count: total}
			} else {
				boundAvgs[iRes] = &pb.TimeSeries{Value: 0, Count: 0}
			}

			if nCols > 1 {
				if total > 0 {
					deciles := computeDeciles(decileCount, dataBuf, bandSize, bandOffset, nodata, dsDscr)
					for ic := 0; ic < len(deciles); ic++ {
						iRes++
						boundAvgs[iRes] = &pb.TimeSeries{Value: float64(deciles[ic]), Count: 1}
					}
				} else {
					for ic := 0; ic < decileCount; ic++ {
						iRes++
						boundAvgs[iRes] = &pb.TimeSeries{Value: 0, Count: 0}
					}
				}
			}
		}

		avgs = append(avgs, boundAvgs[:nCols]...)

		if bandStrides > 2 && len(boundAvgs) > nCols {
			var beta []float64
			var count []float64
			for ic := 0; ic < nCols; ic++ {
				beta_ := (boundAvgs[ic+nCols].Value - boundAvgs[ic].Value) / float64(bandStrides-1)
				beta = append(beta, beta_)

				count_ := math.Round(float64(boundAvgs[ic].Count+boundAvgs[ic+nCols].Count) / float64(2))
				count = append(count, count_)
			}
			for ip := 1; ip < bandStrides-1; ip++ {
				for ic := 0; ic < nCols; ic++ {
					beta_ := beta[ic]
					val := boundAvgs[ic].Value + float64(ip)*beta_
					avgs = append(avgs, &pb.TimeSeries{Value: val, Count: int32(count[ic])})
				}
			}
		}

		if len(boundAvgs) > nCols {
			avgs = append(avgs, boundAvgs[len(boundAvgs)-nCols:]...)
		}

	}
	syscall.Getrusage(syscall.RUSAGE_SELF, &resUsage1)
	metrics.UserTime = resUsage1.Utime.Nano() - resUsage0.Utime.Nano()
	metrics.SysTime = resUsage1.Stime.Nano() - resUsage0.Stime.Nano()

	nRows := len(avgs) / nCols
	return &pb.Result{TimeSeries: avgs, Raster: &pb.Raster{NoData: float64(nodata)}, Shape: []int32{int32(nRows), int32(nCols)}, Error: "OK", Metrics: metrics}
}

func computeDeciles(decileCount int, dataBuf []float32, bandSize int, bandOffset int, nodata float32, dsDscr *DrillFileDescriptor) []float32 {
	deciles := make([]float32, decileCount)

	var buf []float32
	for i := 0; i < bandSize; i++ {
		if dsDscr.Mask[i] == 255 && dataBuf[i+bandOffset] != nodata {
			buf = append(buf, dataBuf[i+bandOffset])
		}
	}

	sort.Slice(buf, func(i, j int) bool { return buf[i] <= buf[j] })
	step := len(buf) / (decileCount + 1)
	if step > 0 {
		isEven := len(buf)%(decileCount+1) == 0

		for i := 0; i < decileCount; i++ {
			iStep := (i + 1) * step
			de := buf[iStep]
			if isEven {
				de = (buf[iStep] + buf[iStep+1]) / 2.0
			}

			deciles[i] = de
		}
	} else {
		padding := make(map[int]int)
		for i := 0; i < decileCount; i++ {
			idx := i % len(buf)
			if _, found := padding[idx]; !found {
				padding[idx] = 0
			}
			padding[idx]++
		}

		idx := 0
		for i := 0; i < len(buf); i++ {
			for p := 0; p < padding[i]; p++ {
				deciles[idx] = buf[i]
				idx++
			}
		}
	}

	return deciles
}

func createMask(ds C.GDALDatasetH, g C.OGRGeometryH, offsetX, offsetY, countX, countY int32) ([]uint8, error) {
	canvas := make([]uint8, countX*countY)

	memStr := fmt.Sprintf("MEM:::DATAPOINTER=%d,PIXELS=%d,LINES=%d,DATATYPE=Byte", unsafe.Pointer(&canvas[0]), countX, countY)
	memStrC := C.CString(memStr)
	defer C.free(unsafe.Pointer(memStrC))
	hDstDS := C.GDALOpen(memStrC, C.GA_Update)
	if hDstDS == nil {
		return nil, fmt.Errorf("Couldn't create memory driver")
	}
	defer C.GDALClose(hDstDS)

	var gdalErr C.CPLErr
	if gdalErr = C.GDALSetProjection(hDstDS, C.GDALGetProjectionRef(ds)); gdalErr != 0 {
		msg := fmt.Errorf("Couldn't set a projection in the mem raster %v", gdalErr)
		log.Println(msg)
		return nil, msg
	}

	geoTrans := make([]float64, 6)
	if gdalErr = C.GDALGetGeoTransform(ds, (*C.double)(&geoTrans[0])); gdalErr != 0 {
		msg := fmt.Errorf("Couldn't get the geotransform from the source dataset %v", gdalErr)
		log.Println(msg)
		return nil, msg
	}

	geoTrans[0] += geoTrans[1] * float64(offsetX)
	geoTrans[3] += geoTrans[5] * float64(offsetY)

	if gdalErr = C.GDALSetGeoTransform(hDstDS, (*C.double)(&geoTrans[0])); gdalErr != 0 {
		msg := fmt.Errorf("Couldn't set the geotransform on the destination dataset %v", gdalErr)
		log.Println(msg)
		return nil, msg
	}

	ic := C.OGR_G_Clone(g)
	defer C.OGR_G_DestroyGeometry(ic)

	geomBurnValue := C.double(255)
	panBandList := []C.int{C.int(1)}
	pahGeomList := []C.OGRGeometryH{ic}

	opts := []*C.char{C.CString("ALL_TOUCHED=TRUE"), nil}
	defer C.free(unsafe.Pointer(opts[0]))

	if gdalErr = C.GDALRasterizeGeometries(hDstDS, 1, &panBandList[0], 1, &pahGeomList[0], nil, nil, &geomBurnValue, &opts[0], nil, nil); gdalErr != 0 {
		msg := fmt.Errorf("GDALRasterizeGeometry error %v", gdalErr)
		log.Println(msg)
		return nil, msg
	}

	return canvas, nil
}

func envelopePolygon(hDS C.GDALDatasetH) (C.OGRGeometryH, error) {
	geoTrans := make([]float64, 6)
	C.GDALGetGeoTransform(hDS, (*C.double)(&geoTrans[0]))

	var ulX, ulY C.double
	C.GDALApplyGeoTransform((*C.double)(&geoTrans[0]), C.double(0), C.double(0), &ulX, &ulY)
	var lrX, lrY C.double
	C.GDALApplyGeoTransform((*C.double)(&geoTrans[0]), C.double(C.GDALGetRasterXSize(hDS)), C.double(C.GDALGetRasterYSize(hDS)), &lrX, &lrY)

	polyWKT := fmt.Sprintf("POLYGON ((%f %f,%f %f,%f %f,%f %f,%f %f))", ulX, ulY,
		ulX, lrY,
		lrX, lrY,
		lrX, ulY,
		ulX, ulY)

	ppszData := C.CString(polyWKT)
	ppszDataTmp := ppszData

	var hGeom C.OGRGeometryH
	hSRS := C.OSRNewSpatialReference(C.GDALGetProjectionRef(hDS))

	// OGR_G_CreateFromWkt intrnally updates &ppszData pointer value
	errC := C.OGR_G_CreateFromWkt(&ppszData, hSRS, &hGeom)

	C.OSRRelease(hSRS)
	C.free(unsafe.Pointer(ppszDataTmp))

	if errC != C.OGRERR_NONE {
		return nil, fmt.Errorf("failed to compute envelope polygon: %v", polyWKT)
	}

	return hGeom, nil
}

func getDrillFileDescriptor(ds C.GDALDatasetH, g C.OGRGeometryH) (*DrillFileDescriptor, error) {
	gCopy := C.OGR_G_Buffer(g, C.double(0.0), C.int(30))
	if C.OGR_G_IsEmpty(gCopy) == C.int(1) {
		gCopy = C.OGR_G_Clone(g)
	}

	defer C.OGR_G_DestroyGeometry(gCopy)

	if C.GoString(C.GDALGetProjectionRef(ds)) != "" {
		desSRS := C.OSRNewSpatialReference(C.GDALGetProjectionRef(ds))
		defer C.OSRDestroySpatialReference(desSRS)
		srcSRS := C.OSRNewSpatialReference(cWGS84WKT)
		defer C.OSRDestroySpatialReference(srcSRS)
		C.OSRSetAxisMappingStrategy(srcSRS, C.OAMS_TRADITIONAL_GIS_ORDER)
		trans := C.OCTNewCoordinateTransformation(srcSRS, desSRS)
		C.OGR_G_Transform(gCopy, trans)
		C.OCTDestroyCoordinateTransformation(trans)
	}

	fileEnv, err := envelopePolygon(ds)
	if err != nil {
		return nil, err
	}
	defer C.OGR_G_DestroyGeometry(fileEnv)

	inters := C.OGR_G_Intersection(gCopy, fileEnv)
	defer C.OGR_G_DestroyGeometry(inters)

	var env C.OGREnvelope
	C.OGR_G_GetEnvelope(inters, &env)

	geot := make([]float64, 6)
	C.GDALGetGeoTransform(ds, (*C.double)(&geot[0]))

	invGeot := make([]float64, 6)
	C.GDALInvGeoTransform((*C.double)(&geot[0]), (*C.double)(&invGeot[0]))

	var offMinX, offMinY, offMaxX, offMaxY C.double
	C.GDALApplyGeoTransform((*C.double)(&invGeot[0]), env.MinX, env.MinY, &offMinX, &offMinY)
	C.GDALApplyGeoTransform((*C.double)(&invGeot[0]), env.MaxX, env.MaxY, &offMaxX, &offMaxY)

	offsetX := int32(math.Min(float64(offMinX), float64(offMaxX)))
	offsetY := int32(math.Min(float64(offMinY), float64(offMaxY)))
	countX := int32(math.Max(float64(offMinX), float64(offMaxX))) - offsetX
	countY := int32(math.Max(float64(offMinY), float64(offMaxY))) - offsetY
	if countX == 0 {
		countX++
	}
	if countY == 0 {
		countY++
	}
	if offsetX < 0 {
		offsetX = 0
	}
	if offsetY < 0 {
		offsetY = 0
	}

	mask, err := createMask(ds, gCopy, offsetX, offsetY, countX, countY)
	return &DrillFileDescriptor{offsetX, offsetY, countX, countY, mask}, err
}
