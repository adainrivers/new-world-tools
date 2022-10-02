package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ake-persson/mapslice-json"
	"github.com/new-world-tools/new-world-tools/datasheet"
	"github.com/new-world-tools/new-world-tools/internal"
	"github.com/new-world-tools/new-world-tools/localization"
	"github.com/new-world-tools/new-world-tools/profiler"
	workerpool "github.com/zelenin/go-worker-pool"
)

const (
	defaultThreads int64 = 5
	maxThreads     int64 = 10
)

var (
	pool             *workerpool.Pool
	localizationData *internal.Store[string]
	outputDir        string
	format           string
	pr               *profiler.Profiler
)

const (
	formatCsv  = "csv"
	formatJson = "json"
)

var formats = map[string]bool{
	formatCsv:  true,
	formatJson: true,
}

func main() {
	pr = profiler.New()

	inputDirPtr := flag.String("input", ".\\extract\\sharedassets\\springboardentitites\\datatables", "directory path")
	localizationDirPtr := flag.String("localization", "", "localization path")
	outputDirPtr := flag.String("output", ".\\datasheets", "directory path")
	formatPtr := flag.String("format", "csv", "csv or json")
	threadsPtr := flag.Int64("threads", defaultThreads, fmt.Sprintf("1-%d", maxThreads))
	flag.Parse()

	format = *formatPtr
	localizationDir := *localizationDirPtr

	if formats[format] != true {
		log.Fatalf("Unsupported format: %s", format)
	}

	threads := *threadsPtr
	if threads < 1 || threads > maxThreads {
		threads = defaultThreads
	}
	log.Printf("The number of threads is set to %d", threads)

	inputDir, err := filepath.Abs(filepath.Clean(*inputDirPtr))
	if err != nil {
		log.Fatalf("filepath.Abs: %s", err)
	}

	_, err = os.Stat(inputDir)
	if os.IsNotExist(err) {
		log.Fatalf("'%s' does not exist", inputDir)
	}

	if localizationDir != "" {
		localizationDir, err = filepath.Abs(filepath.Clean(localizationDir))
		if err != nil {
			log.Fatalf("filepath.Abs: %s", err)
		}

		_, err = os.Stat(localizationDir)
		if os.IsNotExist(err) {
			log.Fatalf("'%s' does not exist", localizationDir)
		}

		localizationData, err = localization.New(localizationDir)
		if err != nil {
			log.Fatalf("localization.New: %s", err)
		}
	}

	outputDir, err = filepath.Abs(filepath.Clean(*outputDirPtr))
	if err != nil {
		log.Fatalf("filepath.Abs: %s", err)
	}

	err = os.MkdirAll(outputDir, 0755)
	if err != nil {
		log.Fatalf("MkdirAll: %s", err)
	}

	files, err := datasheet.FindAll(inputDir)
	if err != nil {
		log.Fatalf("datasheet.FindAll: %s", err)
	}

	pool = workerpool.NewPool(threads, 1000)

	go func() {
		errorChan := pool.Errors()

		for {
			err, ok := <-errorChan
			if !ok {
				break
			}

			taskId := err.(workerpool.TaskError).Id
			err = errors.Unwrap(err)
			log.Printf("task #%d err: %s", taskId, err)
		}
	}()

	keys := map[string]bool{}

	var id int64
	for _, file := range files {
		meta, err := datasheet.ParseMeta(file)
		if err != nil {
			log.Fatalf("datasheet.ParseMeta err: %s", err)
		}
		key := fmt.Sprintf("%s.%s", meta.DataType, meta.UniqueId)
		_, ok := keys[key]
		if ok {
			log.Printf("duplicate key: %q (%s)", key, file.GetPath())
			continue
		}
		keys[key] = true
		id++
		addTask(id, file, inputDir)
	}

	pool.Wait()

	log.Printf("PeakMemory: %0.1fMb Duration: %s", float64(pr.GetPeakMemory())/1024/1024, pr.GetDuration().String())
}

func addTask(id int64, file *datasheet.DataSheetFile, inputDir string) {
	pool.AddTask(workerpool.NewTask(id, func(id int64) error {
		log.Printf("Working: %s", file.GetPath())
		ds, err := datasheet.Parse(file)
		if err != nil {
			return err
		}
		outputFileName := strings.Replace(file.GetPath(), inputDir, "", 1)
		if format == formatCsv {
			outputPath := filepath.Join(outputDir, ds.DataType, fmt.Sprintf("%s.csv", ds.UniqueId))
			err = storeToCsv(ds, outputPath)
			if err != nil {
				return err
			}
		}

		if format == formatJson {
			jsonFileName := strings.Replace(outputFileName, ".datasheet", ".json", 1)
			outputPath := filepath.Join(outputDir, jsonFileName)
			err = storeToJson(ds, outputPath)
			if err != nil {
				return err
			}
		}

		return nil
	}))
}

func storeToCsv(ds *datasheet.DataSheet, path string) error {
	err := os.MkdirAll(filepath.Dir(path), 0755)
	if err != nil {
		return err
	}

	file, err := os.Create(path)
	if err != nil {
		return err
	}

	defer file.Close()

	csvWriter := csv.NewWriter(file)
	csvWriter.UseCRLF = false
	//csvWriter.Comma = ';'

	record := make([]string, len(ds.Columns))
	for i, column := range ds.Columns {
		record[i] = fmt.Sprintf("%s", column.Name)
	}

	err = csvWriter.Write(record)
	if err != nil {
		return err
	}

	for i, row := range ds.Rows {
		record := make([]string, len(row))
		for j, cell := range row {
			record[j] = toString(normalizeCellValue(ds.Columns[j], cell))
		}
		err = csvWriter.Write(record)
		if err != nil {
			return err
		}

		if i%100 == 0 {
			csvWriter.Flush()
		}
	}

	csvWriter.Flush()

	err = csvWriter.Error()
	if err != nil {
		return err
	}

	return nil
}

func storeToJson(ds *datasheet.DataSheet, path string) error {
	err := os.MkdirAll(filepath.Dir(path), 0755)
	if err != nil {
		return err
	}

	file, err := os.Create(path)
	if err != nil {
		return err
	}

	defer file.Close()

	result := make([]mapslice.MapSlice, len(ds.Rows))

	for i, row := range ds.Rows {
		record := make(mapslice.MapSlice, len(row))
		for j, cell := range row {
			record[j] = mapslice.MapItem{
				Key:   fmt.Sprintf("%s", ds.Columns[j].Name),
				Value: normalizeCellValue(ds.Columns[j], cell),
			}
		}

		result[i] = record
	}

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "    ")

	err = encoder.Encode(result)
	if err != nil {
		return err
	}

	return nil
}

func normalizeCellValue(column datasheet.Column, str string) interface{} {
	if column.ColumnType == datasheet.ColumnTypeString {
		return resolveValue(str)
	}

	if column.ColumnType == datasheet.ColumnTypeNumber {
		val, err := strconv.ParseFloat(str, 64)
		if err != nil {
			return nil
		}

		// ugly rounding fix
		pow := math.Pow(10, 6)
		return math.Round(val*pow) / pow
	}

	if column.ColumnType == datasheet.ColumnTypeBoolean {
		val, err := strconv.ParseBool(str)
		if err != nil {
			return nil
		}

		return val
	}

	return str
}

func toString(val interface{}) string {
	str, ok := val.(string)
	if ok {
		return str
	}

	f, ok := val.(float64)
	if ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}

	b, ok := val.(bool)
	if ok {
		return strconv.FormatBool(b)
	}

	if val == nil {
		return ""
	}

	log.Fatalf("not supported value")

	return ""
}

func resolveValue(key string) string {
	if localizationData == nil || !strings.HasPrefix(key, "@") {
		return key
	}

	if localizationData.Has(key) {
		return localizationData.Get(key)
	}

	return key
}
