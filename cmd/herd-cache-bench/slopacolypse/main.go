package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/seveas/herd"
	"github.com/spf13/cobra"

	jsoniter "github.com/json-iterator/go"
	"github.com/bytedance/sonic"
	"github.com/minio/simdjson-go"
	_ "modernc.org/sqlite"
)

type benchResult struct {
	parser   string
	duration time.Duration
	hosts    int
	err      error
}

type cacheFile struct {
	path string
	size int64
}

const toolVersion = "v1.4.0-fts-indexing"

var rootCmd = &cobra.Command{
	Use:   "herd-cache-bench [flags] [cache-dir]",
	Short: "Benchmark JSON parsing performance for Herd cache files",
	Long: `This tool benchmarks different JSON parsing libraries against actual Herd cache files.
It tests: encoding/json (stdlib), jsoniter, sonic, and simdjson-go.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runBenchmark,
}

func init() {
	rootCmd.Flags().StringP("filter", "f", "", "Filter hosts by attribute (e.g., 'app-role=github-lowworker')")
	rootCmd.Flags().BoolP("parse-only", "p", false, "Only parse JSON, don't filter hosts")
	rootCmd.Flags().BoolP("streaming", "s", false, "Use streaming parsers (process hosts one by one)")
	rootCmd.Flags().BoolP("sqlite", "", false, "Test SQLite conversion and queries")
	rootCmd.Flags().BoolP("sqlite-only", "", false, "Only test SQLite (skip JSON parsing)")
	rootCmd.Flags().BoolP("debug-merge", "", false, "Show detailed merge information for hosts")
	rootCmd.Flags().StringP("debug-host", "", "", "Show detailed merge info for specific host")
	rootCmd.Flags().BoolP("json-mode", "", false, "Use simple JSON storage instead of EAV pattern")
	rootCmd.Flags().BoolP("ephemeral", "", false, "Use ephemeral mode - fastest import, no deduplication")
	rootCmd.Flags().BoolP("use-jsoniter", "", false, "Use jsoniter instead of stdlib for JSON parsing")
	rootCmd.Flags().BoolP("force-import", "", false, "Force database rebuild even if file exists")
	rootCmd.Flags().BoolP("fts-index", "", false, "Create FTS index for attribute searching")
	rootCmd.Flags().BoolP("json-index", "", false, "Create JSON indexes for specific attributes")
	rootCmd.Flags().BoolP("full-denorm", "", false, "Fully denormalize - create columns for ALL attributes (no JSON)")
	rootCmd.Flags().BoolP("bulk-insert", "", false, "Use bulk INSERT for better performance")
	rootCmd.Flags().BoolP("smart-hybrid", "", false, "Smart hybrid: top 20 attributes as columns, rest as JSON")
	rootCmd.Flags().StringP("multi-filter", "", "", "Multiple filters separated by commas (e.g., 'app=web,region=us-east')")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runBenchmark(cmd *cobra.Command, args []string) error {
	cacheDir := filepath.Join(os.Getenv("HOME"), ".cache", "herd")
	if len(args) > 0 {
		cacheDir = args[0]
	}

	// Find all cache files
	files, err := findCacheFiles(cacheDir)
	if err != nil {
		return fmt.Errorf("error finding cache files: %w", err)
	}

	if len(files) == 0 {
		return fmt.Errorf("no cache files found in %s", cacheDir)
	}

	filter, _ := cmd.Flags().GetString("filter")
	parseOnly, _ := cmd.Flags().GetBool("parse-only")
	streaming, _ := cmd.Flags().GetBool("streaming")
	useSQLite, _ := cmd.Flags().GetBool("sqlite")
	sqliteOnly, _ := cmd.Flags().GetBool("sqlite-only")

	fmt.Printf("herd-cache-bench %s\n", toolVersion)
	fmt.Printf("Found %d cache files in %s\n", len(files), cacheDir)
	fmt.Printf("Total size: %.2f MB\n", float64(totalSize(files))/(1024*1024))
	
	if filter != "" {
		fmt.Printf("Filter: %s\n", filter)
	}
	if parseOnly {
		fmt.Printf("Mode: Parse only (no filtering)\n")
	} else if streaming {
		fmt.Printf("Mode: Streaming parse + filter (early termination)\n")
	} else if useSQLite || sqliteOnly {
		fmt.Printf("Mode: SQLite conversion and queries\n")
	} else {
		fmt.Printf("Mode: Parse + filter hosts\n")
	}
	fmt.Println()

	// Handle SQLite mode
	if useSQLite || sqliteOnly {
		debugMerge, _ := cmd.Flags().GetBool("debug-merge")
		debugHost, _ := cmd.Flags().GetString("debug-host")
		jsonMode, _ := cmd.Flags().GetBool("json-mode")
		ephemeral, _ := cmd.Flags().GetBool("ephemeral")
		useJsoniter, _ := cmd.Flags().GetBool("use-jsoniter")
		forceImport, _ := cmd.Flags().GetBool("force-import")
		ftsIndex, _ := cmd.Flags().GetBool("fts-index")
		jsonIndex, _ := cmd.Flags().GetBool("json-index")
		fullDenorm, _ := cmd.Flags().GetBool("full-denorm")
		return runSQLiteBenchmark(files, filter, parseOnly, sqliteOnly, debugMerge, debugHost, jsonMode, ephemeral, useJsoniter, forceImport, ftsIndex, jsonIndex, fullDenorm)
	}

	// Check CPU support
	fmt.Printf("System info:\n")
	fmt.Printf("- CPU: %s\n", runtime.GOARCH)
	fmt.Printf("- Cores: %d\n", runtime.NumCPU())
	fmt.Printf("- simdjson support: %v\n", simdjson.SupportedCPU())
	fmt.Printf("- sonic support: %v\n\n", checkSonicSupport())

	// Run benchmarks
	parsers := []string{"stdlib", "jsoniter", "sonic", "simdjson"}
	
	// Warm up - read files into memory
	fmt.Println("Warming up file cache...")
	for _, f := range files {
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		_ = data
	}

	fmt.Println("\nRunning benchmarks (parsing all cache files)...")
	fmt.Println(strings.Repeat("-", 80))
	
	results := make(map[string][]benchResult)
	iterations := 3

	for i := 0; i < iterations; i++ {
		fmt.Printf("\nIteration %d/%d:\n", i+1, iterations)
		
		for _, parser := range parsers {
			result := benchmarkParser(parser, files, filter, parseOnly, streaming)
			results[parser] = append(results[parser], result)
			
			fmt.Printf("  %-10s: %10s", parser, result.duration.Round(time.Millisecond))
			if result.err != nil {
				fmt.Printf(" (ERROR: %v)", result.err)
			} else {
				fmt.Printf(" (%d hosts)", result.hosts)
			}
			fmt.Println()
		}
	}

	// Print summary
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("SUMMARY (average of", iterations, "iterations):")
	fmt.Println(strings.Repeat("-", 80))
	
	type summary struct {
		parser   string
		avgTime  time.Duration
		speedup  float64
		hosts    int
	}
	
	summaries := []summary{}
	var baseTime time.Duration
	
	for _, parser := range parsers {
		var totalDuration time.Duration
		var totalHosts int
		validRuns := 0
		
		for _, r := range results[parser] {
			if r.err == nil {
				totalDuration += r.duration
				totalHosts = r.hosts // Should be same for all runs
				validRuns++
			}
		}
		
		if validRuns > 0 {
			avg := totalDuration / time.Duration(validRuns)
			s := summary{
				parser:  parser,
				avgTime: avg,
				hosts:   totalHosts,
			}
			
			if parser == "stdlib" {
				baseTime = avg
				s.speedup = 1.0
			} else if baseTime > 0 {
				s.speedup = float64(baseTime) / float64(avg)
			}
			
			summaries = append(summaries, s)
		}
	}
	
	// Sort by speed
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].avgTime < summaries[j].avgTime
	})
	
	fmt.Printf("%-12s %12s %10s %12s\n", "Parser", "Avg Time", "Speedup", "Total Hosts")
	fmt.Println(strings.Repeat("-", 50))
	
	for _, s := range summaries {
		fmt.Printf("%-12s %12s %9.2fx %12d\n", 
			s.parser, 
			s.avgTime.Round(time.Millisecond),
			s.speedup,
			s.hosts,
		)
	}
	
	return nil
}

func findCacheFiles(dir string) ([]cacheFile, error) {
	var files []cacheFile
	
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".cache") {
			continue
		}
		
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		
		files = append(files, cacheFile{
			path: path,
			size: info.Size(),
		})
	}
	
	return files, nil
}

func totalSize(files []cacheFile) int64 {
	var total int64
	for _, f := range files {
		total += f.size
	}
	return total
}

func benchmarkParser(parser string, files []cacheFile, filter string, parseOnly bool, streaming bool) benchResult {
	start := time.Now()
	totalHosts := 0
	
	for _, f := range files {
		hosts, err := parseFile(parser, f.path, filter, parseOnly, streaming)
		if err != nil {
			return benchResult{
				parser:   parser,
				duration: time.Since(start),
				err:      err,
			}
		}
		totalHosts += hosts
	}
	
	return benchResult{
		parser:   parser,
		duration: time.Since(start),
		hosts:    totalHosts,
	}
}

func parseFile(parser string, path string, filter string, parseOnly bool, streaming bool) (int, error) {
	if streaming {
		switch parser {
		case "stdlib":
			return parseStdlibStreaming(path, filter, parseOnly)
		case "jsoniter":
			return parseJsoniterStreaming(path, filter, parseOnly)
		case "sonic":
			return parseSonicStreaming(path, filter, parseOnly)
		case "simdjson":
			return parseSimdjsonStreaming(path, filter, parseOnly)
		default:
			return 0, fmt.Errorf("unknown parser: %s", parser)
		}
	}
	
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	
	switch parser {
	case "stdlib":
		return parseStdlib(data, filter, parseOnly)
	case "jsoniter":
		return parseJsoniter(data, filter, parseOnly)
	case "sonic":
		return parseSonic(data, filter, parseOnly)
	case "simdjson":
		return parseSimdjson(data, filter, parseOnly)
	default:
		return 0, fmt.Errorf("unknown parser: %s", parser)
	}
}

func parseStdlib(data []byte, filter string, parseOnly bool) (int, error) {
	var hosts herd.HostSet
	if err := json.Unmarshal(data, &hosts); err != nil {
		return 0, err
	}
	
	if parseOnly {
		return hosts.Len(), nil
	}
	
	return filterHosts(&hosts, filter), nil
}

func parseJsoniter(data []byte, filter string, parseOnly bool) (int, error) {
	var hosts herd.HostSet
	json := jsoniter.ConfigCompatibleWithStandardLibrary
	if err := json.Unmarshal(data, &hosts); err != nil {
		return 0, err
	}
	
	if parseOnly {
		return hosts.Len(), nil
	}
	
	return filterHosts(&hosts, filter), nil
}

func parseSonic(data []byte, filter string, parseOnly bool) (int, error) {
	if !checkSonicSupport() {
		return 0, fmt.Errorf("sonic not supported on this CPU")
	}
	
	var hosts herd.HostSet
	if err := sonic.Unmarshal(data, &hosts); err != nil {
		return 0, err
	}
	
	if parseOnly {
		return hosts.Len(), nil
	}
	
	return filterHosts(&hosts, filter), nil
}

func parseSimdjson(data []byte, filter string, parseOnly bool) (int, error) {
	if !simdjson.SupportedCPU() {
		return 0, fmt.Errorf("simdjson not supported on this CPU")
	}
	
	// simdjson validates and parses, then we fall back to stdlib for unmarshaling
	// This measures simdjson's parsing speed + stdlib's unmarshaling speed
	_, err := simdjson.Parse(data, nil)
	if err != nil {
		return 0, err
	}
	
	// For fair comparison, unmarshal to the same struct as other parsers
	var hosts herd.HostSet
	if err := json.Unmarshal(data, &hosts); err != nil {
		return 0, err
	}
	
	if parseOnly {
		return hosts.Len(), nil
	}
	
	return filterHosts(&hosts, filter), nil
}

func checkSonicSupport() bool {
	// Test sonic with simple JSON to check support
	testData := []byte(`{"test":true}`)
	var result map[string]interface{}
	err := sonic.Unmarshal(testData, &result)
	return err == nil
}

func filterHosts(hosts *herd.HostSet, filter string) int {
	if filter == "" {
		return hosts.Len()
	}
	
	// Parse filter like "app-role=github-lowworker"
	parts := strings.SplitN(filter, "=", 2)
	if len(parts) != 2 {
		return hosts.Len() // Invalid filter, return all
	}
	
	key, value := parts[0], parts[1]
	count := 0
	
	// Count hosts that match the filter
	for i := 0; i < hosts.Len(); i++ {
		host := hosts.Get(i)
		if attrs, ok := host.Attributes[key]; ok {
			// Handle both string and slice values
			switch v := attrs.(type) {
			case string:
				if v == value {
					count++
				}
			case []interface{}:
				for _, item := range v {
					if str, ok := item.(string); ok && str == value {
						count++
						break
					}
				}
			case []string:
				for _, str := range v {
					if str == value {
						count++
						break
					}
				}
			}
		}
	}
	
	return count
}

// Streaming implementations that process hosts one by one

func parseStdlibStreaming(path string, filter string, parseOnly bool) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	
	// HostSet marshals as an array of hosts, not an object
	// Read opening bracket
	token, err := decoder.Token()
	if err != nil {
		return 0, err
	}
	if token != json.Delim('[') {
		return 0, fmt.Errorf("expected array, got %v", token)
	}
	
	count := 0
	totalHosts := 0
	
	// Process each host in the array
	for decoder.More() {
		var host herd.Host
		if err := decoder.Decode(&host); err != nil {
			return 0, err
		}
		
		totalHosts++
		
		if parseOnly {
			count++
		} else if matchesFilter(&host, filter) {
			count++
		}
	}
	
	// Read closing bracket
	if _, err := decoder.Token(); err != nil {
		return 0, err
	}
	
	if parseOnly {
		return totalHosts, nil
	}
	return count, nil
}

func parseJsoniterStreaming(path string, filter string, parseOnly bool) (int, error) {
	// jsoniter's streaming API is different, fall back to stdlib streaming for now
	return parseStdlibStreaming(path, filter, parseOnly)
}

func parseSonicStreaming(path string, filter string, parseOnly bool) (int, error) {
	if !checkSonicSupport() {
		return 0, fmt.Errorf("sonic not supported on this CPU")
	}
	
	// Sonic doesn't have streaming decoder, fall back to stdlib streaming
	return parseStdlibStreaming(path, filter, parseOnly)
}

func parseSimdjsonStreaming(path string, filter string, parseOnly bool) (int, error) {
	if !simdjson.SupportedCPU() {
		return 0, fmt.Errorf("simdjson not supported on this CPU")
	}
	
	// simdjson doesn't have direct streaming to structs, fall back to stdlib streaming
	return parseStdlibStreaming(path, filter, parseOnly)
}

func matchesFilter(host *herd.Host, filter string) bool {
	if filter == "" {
		return true
	}
	
	// Parse filter like "app-role=github-lowworker"
	parts := strings.SplitN(filter, "=", 2)
	if len(parts) != 2 {
		return true // Invalid filter, return all
	}
	
	key, value := parts[0], parts[1]
	
	if attrs, ok := host.Attributes[key]; ok {
		// Handle both string and slice values
		switch v := attrs.(type) {
		case string:
			return v == value
		case []interface{}:
			for _, item := range v {
				if str, ok := item.(string); ok && str == value {
					return true
				}
			}
		case []string:
			for _, str := range v {
				if str == value {
					return true
				}
			}
		}
	}
	
	return false
}

// SQLite benchmark functions

func runSQLiteBenchmark(files []cacheFile, filter string, parseOnly bool, sqliteOnly bool, debugMerge bool, debugHost string, jsonMode bool, ephemeral bool, useJsoniter bool, forceImport bool, ftsIndex bool, jsonIndex bool, fullDenorm bool) error {
	// Place database in the same directory as cache files
	cacheDir := filepath.Join(os.Getenv("HOME"), ".cache", "herd")
	dbPath := filepath.Join(cacheDir, "benchmark_cache.db")
	
	// Don't delete - let it persist for inspection
	fmt.Printf("Database will be stored at: %s\n", dbPath)
	
	if !sqliteOnly {
		fmt.Println("=== SQLite Conversion Benchmark ===")
		
		// Benchmark conversion from JSON to SQLite
		start := time.Now()
		var totalHosts int
		var err error
		if fullDenorm {
			totalHosts, err = convertJSONToSQLiteFullDenorm(files, dbPath, debugMerge, debugHost, useJsoniter, ftsIndex, jsonIndex)
		} else if ephemeral || jsonMode {
			totalHosts, err = convertJSONToSQLiteEphemeral(files, dbPath, debugMerge, debugHost, useJsoniter, ftsIndex, jsonIndex)
		} else {
			totalHosts, err = convertJSONToSQLite(files, dbPath, debugMerge, debugHost, useJsoniter, ftsIndex, jsonIndex)
		}
		conversionTime := time.Since(start)
		
		if err != nil {
			return fmt.Errorf("SQLite conversion failed: %w", err)
		}
		
		// Get database file size
		dbInfo, err := os.Stat(dbPath)
		if err != nil {
			return fmt.Errorf("failed to get database size: %w", err)
		}
		
		fmt.Printf("Conversion completed:\n")
		fmt.Printf("- Time: %v\n", conversionTime.Round(time.Millisecond))
		fmt.Printf("- Hosts converted: %d\n", totalHosts)
		fmt.Printf("- Database size: %.2f MB\n", float64(dbInfo.Size())/(1024*1024))
		fmt.Printf("- Compression ratio: %.1fx\n", float64(totalSize(files))/float64(dbInfo.Size()))
		fmt.Println()
	} else {
		// For sqlite-only mode, check if we need to create/rebuild the database
		if forceImport || func() bool {
			if _, err := os.Stat(dbPath); os.IsNotExist(err) {
				return true // File doesn't exist, need to import
			}
			return false // File exists and no force import
		}() {
			fmt.Println("=== SQLite Conversion (for sqlite-only mode) ===")
			start := time.Now()
			var totalHosts int
			var err error
			if fullDenorm {
				totalHosts, err = convertJSONToSQLiteFullDenorm(files, dbPath, debugMerge, debugHost, useJsoniter, ftsIndex, jsonIndex)
			} else if ephemeral || jsonMode {
				totalHosts, err = convertJSONToSQLiteEphemeral(files, dbPath, debugMerge, debugHost, useJsoniter, ftsIndex, jsonIndex)
			} else {
				totalHosts, err = convertJSONToSQLite(files, dbPath, debugMerge, debugHost, useJsoniter, ftsIndex, jsonIndex)
			}
			conversionTime := time.Since(start)
			
			if err != nil {
				return fmt.Errorf("SQLite conversion failed: %w", err)
			}
			
			// Get database file size
			dbInfo, err := os.Stat(dbPath)
			if err != nil {
				return fmt.Errorf("failed to get database size: %w", err)
			}
			
			fmt.Printf("Conversion completed:\n")
			fmt.Printf("- Time: %v\n", conversionTime.Round(time.Millisecond))
			fmt.Printf("- Hosts converted: %d\n", totalHosts)
			fmt.Printf("- Database size: %.2f MB\n", float64(dbInfo.Size())/(1024*1024))
			fmt.Printf("- Compression ratio: %.1fx\n", float64(totalSize(files))/float64(dbInfo.Size()))
			fmt.Println()
		} else {
			fmt.Println("Using existing SQLite database\n")
		}
	}
	
	fmt.Println("=== SQLite Query Benchmark ===")
	
	// Debug: Show some sample attributes
	if filter != "" && !jsonMode && !ephemeral {
		fmt.Printf("DEBUG: Looking for filter '%s'\n", filter)
		db, err := sql.Open("sqlite", dbPath)
		if err == nil {
			parts := strings.SplitN(filter, "=", 2)
			if len(parts) == 2 {
				// Check if attributes table exists
				var tableExists int
				db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='attributes'").Scan(&tableExists)
				
				if tableExists > 0 {
					var count int
					db.QueryRow("SELECT COUNT(*) FROM attributes WHERE key = ?", parts[0]).Scan(&count)
					fmt.Printf("DEBUG: Found %d rows with key '%s'\n", count, parts[0])
					
					rows, err := db.Query("SELECT DISTINCT value FROM attributes WHERE key = ? ORDER BY value", parts[0])
					if err == nil {
						fmt.Printf("DEBUG: All values for '%s':\n", parts[0])
						for rows.Next() {
							var value string
							rows.Scan(&value)
							fmt.Printf("  - %s\n", value)
						}
						rows.Close()
						
						// Check if our target value exists
						var exactCount int
						db.QueryRow("SELECT COUNT(*) FROM attributes WHERE key = ? AND value = ?", parts[0], parts[1]).Scan(&exactCount)
						fmt.Printf("DEBUG: Exact matches for '%s=%s': %d\n", parts[0], parts[1], exactCount)
					}
				}
			}
			db.Close()
		}
		fmt.Println()
	}
	
	// Test different query types
	var queries []struct {
		name        string
		description string
		testFunc    func(*sql.DB, string) (int, time.Duration, error)
	}
	
	if fullDenorm {
		queries = []struct {
			name        string
			description string
			testFunc    func(*sql.DB, string) (int, time.Duration, error)
		}{
			{"count_all", "Count all hosts", querySQLiteCountAll},
			{"full_denorm", "Filter using fully denormalized columns (ULTIMATE!)", querySQLiteFullDenorm},
		}
	} else if jsonMode || ephemeral {
		if ftsIndex {
			queries = []struct {
				name        string
				description string
				testFunc    func(*sql.DB, string) (int, time.Duration, error)
			}{
				{"count_all", "Count all hosts", querySQLiteCountAll},
				{"fts_search", "Filter using FTS", querySQLiteFTS},
				{"json_extract", "Filter using json_extract", querySQLiteJSONExtract},
				{"like_search", "Filter using LIKE on JSON", querySQLiteJSONLike},
			}
		} else {
			queries = []struct {
				name        string
				description string
				testFunc    func(*sql.DB, string) (int, time.Duration, error)
			}{
				{"count_all", "Count all hosts", querySQLiteCountAll},
				{"denorm_filter", "Filter using denormalized columns (FAST!)", querySQLiteDenormalized},
				{"json_extract", "Filter using json_extract", querySQLiteJSONExtract},
				{"json_multi", "Filter using multiple JSON indexes", querySQLiteJSONMulti},
				{"json_arrow", "Filter using ->> operator", querySQLiteJSONArrow},
				{"like_search", "Filter using LIKE on JSON", querySQLiteJSONLike},
				{"bulk_load_json", "Load filtered hosts with JSON", querySQLiteJSONBulkLoad},
			}
		}
	} else {
		queries = []struct {
			name        string
			description string
			testFunc    func(*sql.DB, string) (int, time.Duration, error)
		}{
			{"count_all", "Count all hosts", querySQLiteCountAll},
			{"filter_attr", "Filter by attribute", querySQLiteFilter},
			{"bulk_load", "Load all hosts", querySQLiteBulkLoad},
		}
	}
	
	for _, query := range queries {
		fmt.Printf("Testing %s (%s):\n", query.name, query.description)
		
		for i := 0; i < 3; i++ {
			db, err := sql.Open("sqlite", dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			
			count, duration, err := query.testFunc(db, filter)
			db.Close()
			
			if err != nil {
				fmt.Printf("  Iteration %d: ERROR: %v\n", i+1, err)
			} else {
				fmt.Printf("  Iteration %d: %v (%d results)\n", i+1, duration.Round(time.Microsecond), count)
			}
		}
		fmt.Println()
	}
	
	return nil
}

func convertJSONToSQLiteEphemeral(files []cacheFile, dbPath string, debugMerge bool, debugHost string, useJsoniter bool, ftsIndex bool, jsonIndex bool) (int, error) {
	totalStart := time.Now()
	
	// Database setup timing
	setupStart := time.Now()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	
	// Ephemeral mode - optimize for speed, not durability
	fastMode := `
	PRAGMA journal_mode = MEMORY;
	PRAGMA synchronous = OFF;
	PRAGMA cache_size = 100000;
	PRAGMA temp_store = MEMORY;
	PRAGMA foreign_keys = OFF;
	
	-- Drop and recreate table (fresh start every time)
	DROP TABLE IF EXISTS hosts;
	
	CREATE TABLE hosts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT,
		address TEXT,
		provider TEXT,
		attributes_json TEXT,
		-- Denormalized columns for fast queries
		app_role TEXT,
		region TEXT,
		site TEXT,
		app TEXT,
		role TEXT,
		stamp TEXT
	);
	`
	
	if _, err := db.Exec(fastMode); err != nil {
		return 0, fmt.Errorf("failed to setup fast mode: %w", err)
	}
	
	// Single prepared statement for bulk insertion with denormalized columns
	hostStmt, err := db.Prepare("INSERT INTO hosts (name, address, provider, attributes_json, app_role, region, site, app, role, stamp) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return 0, err
	}
	defer hostStmt.Close()
	
	setupTime := time.Since(setupStart)
	
	// Begin transaction
	txStart := time.Now()
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	txSetupTime := time.Since(txStart)
	
	totalHosts := 0
	hostsPerFile := make(map[string]int)
	var totalReadTime, totalParseTime, totalInsertTime time.Duration
	
	// Setup JSON parser
	var jsonAPI jsoniter.API
	if useJsoniter {
		jsonAPI = jsoniter.ConfigCompatibleWithStandardLibrary
		fmt.Printf("\nUsing jsoniter for JSON parsing\n")
	} else {
		fmt.Printf("\nUsing stdlib for JSON parsing\n")
	}
	
	fmt.Printf("\nDetailed timing breakdown:\n")
	
	// Process each file and just insert everything (no merging!)
	for _, file := range files {
		// File reading timing
		readStart := time.Now()
		data, err := os.ReadFile(file.path)
		if err != nil {
			return 0, err
		}
		readTime := time.Since(readStart)
		totalReadTime += readTime
		
		// JSON parsing timing
		parseStart := time.Now()
		var hostSet herd.HostSet
		if useJsoniter {
			if err := jsonAPI.Unmarshal(data, &hostSet); err != nil {
				return 0, err
			}
		} else {
			if err := json.Unmarshal(data, &hostSet); err != nil {
				return 0, err
			}
		}
		parseTime := time.Since(parseStart)
		totalParseTime += parseTime
		
		provider := strings.TrimSuffix(filepath.Base(file.path), ".cache")
		hostsPerFile[provider] = hostSet.Len()
		
		// Insertion timing
		insertStart := time.Now()
		for i := 0; i < hostSet.Len(); i++ {
			host := hostSet.Get(i)
			
			attrJSON, _ := json.Marshal(host.Attributes)
			
			// Extract common attributes for denormalized columns
			appRole := getStringAttr(host.Attributes, "app-role")
			region := getStringAttr(host.Attributes, "region")
			site := getStringAttr(host.Attributes, "site")
			app := getStringAttr(host.Attributes, "app")
			role := getStringAttr(host.Attributes, "role")
			stamp := getStringAttr(host.Attributes, "stamp")
			
			if _, err := tx.Stmt(hostStmt).Exec(host.Name, host.Address, provider, string(attrJSON), appRole, region, site, app, role, stamp); err != nil {
				return 0, fmt.Errorf("failed to insert host %s: %w", host.Name, err)
			}
			
			totalHosts++
		}
		insertTime := time.Since(insertStart)
		totalInsertTime += insertTime
		
		fmt.Printf("- %s: read %v, parse %v, insert %v (%d hosts)\n", 
			provider, readTime.Round(time.Millisecond), parseTime.Round(time.Millisecond), 
			insertTime.Round(time.Millisecond), hostSet.Len())
	}
	
	// Commit timing
	commitStart := time.Now()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	commitTime := time.Since(commitStart)
	
	totalTime := time.Since(totalStart)
	
	// Show detailed timing stats
	fmt.Printf("\nTiming summary:\n")
	fmt.Printf("- Database setup: %v\n", setupTime.Round(time.Millisecond))
	fmt.Printf("- Transaction setup: %v\n", txSetupTime.Round(time.Millisecond))
	fmt.Printf("- File reading: %v\n", totalReadTime.Round(time.Millisecond))
	fmt.Printf("- JSON parsing: %v\n", totalParseTime.Round(time.Millisecond))
	fmt.Printf("- Data insertion: %v\n", totalInsertTime.Round(time.Millisecond))
	fmt.Printf("- Transaction commit: %v\n", commitTime.Round(time.Millisecond))
	fmt.Printf("- Total time: %v\n", totalTime.Round(time.Millisecond))
	
	// Show data stats
	fmt.Printf("\nData summary:\n")
	for provider, count := range hostsPerFile {
		fmt.Printf("- Hosts in %s: %d\n", provider, count)
	}
	fmt.Printf("- Total records inserted: %d\n", totalHosts)
	
	// Index creation timing
	var indexTime time.Duration
	if ftsIndex || jsonIndex {
		indexStart := time.Now()
		
		if ftsIndex {
			fmt.Printf("- Creating FTS index...\n")
			// Create FTS virtual table for searching attributes
			ftsSQL := `
			CREATE VIRTUAL TABLE IF NOT EXISTS hosts_fts USING fts5(
				name,
				attributes_searchable,
				content='hosts',
				content_rowid='id'
			)`
			if _, err := db.Exec(ftsSQL); err != nil {
				return 0, fmt.Errorf("failed to create FTS table: %w", err)
			}
			
			// Populate FTS with searchable attribute data
			populateSQL := `
			INSERT INTO hosts_fts(rowid, name, attributes_searchable)
			SELECT id, name, 
				   replace(replace(attributes_json, '"', ''), '{', '') || ' ' ||
				   replace(replace(replace(replace(attributes_json, '":', '='), '","', ' '), '{', ''), '}', '')
			FROM hosts`
			if _, err := db.Exec(populateSQL); err != nil {
				return 0, fmt.Errorf("failed to populate FTS: %w", err)
			}
		}
		
		if jsonIndex {
			fmt.Printf("- Creating indexes on denormalized columns (much faster!)...\n")
			
			// Create indexes on denormalized columns - these will be MUCH faster
			denormIndexes := []string{
				"CREATE INDEX IF NOT EXISTS idx_app_role ON hosts(app_role)",
				"CREATE INDEX IF NOT EXISTS idx_region ON hosts(region)", 
				"CREATE INDEX IF NOT EXISTS idx_site ON hosts(site)",
				"CREATE INDEX IF NOT EXISTS idx_app ON hosts(app)",
				"CREATE INDEX IF NOT EXISTS idx_role ON hosts(role)",
				"CREATE INDEX IF NOT EXISTS idx_stamp ON hosts(stamp)",
				// Compound indexes for multi-attribute queries
				"CREATE INDEX IF NOT EXISTS idx_app_role_region ON hosts(app_role, region)",
				"CREATE INDEX IF NOT EXISTS idx_app_role_site ON hosts(app_role, site)",
				"CREATE INDEX IF NOT EXISTS idx_app_region ON hosts(app, region)",
				"CREATE INDEX IF NOT EXISTS idx_role_region ON hosts(role, region)",
				"CREATE INDEX IF NOT EXISTS idx_site_region ON hosts(site, region)",
			}
			
			for _, indexSQL := range denormIndexes {
				if _, err := db.Exec(indexSQL); err != nil {
					fmt.Printf("  Warning: failed to create index: %v\n", err)
				} else {
					fmt.Printf("  Created denormalized index\n")
				}
			}
		}
		
		indexTime = time.Since(indexStart)
	} else {
		fmt.Printf("- Skipping indexes for maximum speed\n")
	}
	
	// Update timing summary to include index time
	totalTime = time.Since(totalStart)
	
	fmt.Printf("\nFinal timing summary:\n")
	fmt.Printf("- Database setup: %v\n", setupTime.Round(time.Millisecond))
	fmt.Printf("- Transaction setup: %v\n", txSetupTime.Round(time.Millisecond))
	fmt.Printf("- File reading: %v\n", totalReadTime.Round(time.Millisecond))
	fmt.Printf("- JSON parsing: %v\n", totalParseTime.Round(time.Millisecond))
	fmt.Printf("- Data insertion: %v\n", totalInsertTime.Round(time.Millisecond))
	fmt.Printf("- Transaction commit: %v\n", commitTime.Round(time.Millisecond))
	if indexTime > 0 {
		fmt.Printf("- Index creation: %v\n", indexTime.Round(time.Millisecond))
	}
	fmt.Printf("- Total time: %v\n", totalTime.Round(time.Millisecond))
	
	fmt.Println()
	
	return totalHosts, nil
}

func convertJSONToSQLiteFullDenorm(files []cacheFile, dbPath string, debugMerge bool, debugHost string, useJsoniter bool, ftsIndex bool, jsonIndex bool) (int, error) {
	totalStart := time.Now()
	
	fmt.Printf("\n=== FULL DENORMALIZATION MODE ===\n")
	fmt.Printf("Creating columns for ALL attributes (no JSON storage)\n\n")
	
	// PHASE 1: Discover all unique attribute keys
	fmt.Printf("Phase 1: Discovering all unique attributes...\n")
	discoverStart := time.Now()
	
	allAttributes := make(map[string]bool)
	var totalHostsScanned int
	
	// Setup JSON parser
	var jsonAPI jsoniter.API
	if useJsoniter {
		jsonAPI = jsoniter.ConfigCompatibleWithStandardLibrary
		fmt.Printf("Using jsoniter for JSON parsing\n")
	} else {
		fmt.Printf("Using stdlib for JSON parsing\n")
	}
	
	for _, file := range files {
		data, err := os.ReadFile(file.path)
		if err != nil {
			return 0, err
		}
		
		var hostSet herd.HostSet
		if useJsoniter {
			if err := jsonAPI.Unmarshal(data, &hostSet); err != nil {
				return 0, err
			}
		} else {
			if err := json.Unmarshal(data, &hostSet); err != nil {
				return 0, err
			}
		}
		
		provider := strings.TrimSuffix(filepath.Base(file.path), ".cache")
		
		for i := 0; i < hostSet.Len(); i++ {
			host := hostSet.Get(i)
			totalHostsScanned++
			
			for key := range host.Attributes {
				allAttributes[key] = true
			}
		}
		
		fmt.Printf("- %s: %d hosts, discovered %d unique attributes so far\n", 
			provider, hostSet.Len(), len(allAttributes))
	}
	
	discoverTime := time.Since(discoverStart)
	
	// Convert to sorted slice for deterministic schema
	var attributeNames []string
	for attr := range allAttributes {
		attributeNames = append(attributeNames, attr)
	}
	sort.Strings(attributeNames)
	
	fmt.Printf("\nDiscovery completed in %v\n", discoverTime.Round(time.Millisecond))
	fmt.Printf("- Total hosts scanned: %d\n", totalHostsScanned)
	fmt.Printf("- Unique attributes found: %d\n", len(attributeNames))
	fmt.Printf("- Attributes: %v\n", attributeNames[:min(len(attributeNames), 10)]) // Show first 10
	if len(attributeNames) > 10 {
		fmt.Printf("  ... and %d more\n", len(attributeNames)-10)
	}
	fmt.Println()
	
	// PHASE 2: Create database with dynamic schema
	fmt.Printf("Phase 2: Creating dynamic schema...\n")
	schemaStart := time.Now()
	
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	
	// Fast mode settings
	fastMode := `
	PRAGMA journal_mode = MEMORY;
	PRAGMA synchronous = OFF;
	PRAGMA cache_size = 100000;
	PRAGMA temp_store = MEMORY;
	PRAGMA foreign_keys = OFF;
	
	DROP TABLE IF EXISTS hosts;
	`
	
	if _, err := db.Exec(fastMode); err != nil {
		return 0, fmt.Errorf("failed to setup fast mode: %w", err)
	}
	
	// Build CREATE TABLE statement with all discovered attributes
	createSQL := `CREATE TABLE hosts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT,
		address TEXT,
		provider TEXT`
	
	// Add column for each discovered attribute (avoid duplicates)
	usedColumns := map[string]bool{
		"id": true, "name": true, "address": true, "provider": true,
	}
	
	// Track which attributes actually get columns
	var actualAttributes []string
	var actualColumnNames []string
	
	for _, attr := range attributeNames {
		// Sanitize column name (replace special characters)
		columnName := strings.ReplaceAll(attr, "-", "_")
		columnName = strings.ReplaceAll(columnName, ".", "_")
		columnName = strings.ReplaceAll(columnName, ":", "_")
		
		// Skip if we already have this column name
		if usedColumns[columnName] {
			fmt.Printf("  Skipping duplicate column: %s (from attribute %s)\n", columnName, attr)
			continue
		}
		usedColumns[columnName] = true
		
		actualAttributes = append(actualAttributes, attr)
		actualColumnNames = append(actualColumnNames, columnName)
		
		quotedColumnName := "`" + columnName + "`" // Quote it to be safe
		createSQL += ",\n\t\t" + quotedColumnName + " TEXT"
	}
	createSQL += "\n\t);"
	
	if _, err := db.Exec(createSQL); err != nil {
		return 0, fmt.Errorf("failed to create table: %w", err)
	}
	
	schemaTime := time.Since(schemaStart)
	fmt.Printf("Schema created in %v\n", schemaTime.Round(time.Millisecond))
	fmt.Printf("- Columns created: %d (id, name, address, provider + %d attributes)\n", 
		4+len(actualAttributes), len(actualAttributes))
	fmt.Printf("- Actual attribute columns: %v\n", actualColumnNames[:min(len(actualColumnNames), 10)])
	if len(actualColumnNames) > 10 {
		fmt.Printf("  ... and %d more\n", len(actualColumnNames)-10)
	}
	fmt.Println()
	
	// PHASE 3: Build prepared statement and import data
	fmt.Printf("Phase 3: Importing data...\n")
	importStart := time.Now()
	
	// Build INSERT statement using only the actual columns
	insertSQL := "INSERT INTO hosts (name, address, provider"
	valuesSQL := "VALUES (?, ?, ?"
	
	for _, columnName := range actualColumnNames {
		quotedColumnName := "`" + columnName + "`"
		insertSQL += ", " + quotedColumnName
		valuesSQL += ", ?"
	}
	insertSQL += ") " + valuesSQL + ")"
	
	hostStmt, err := db.Prepare(insertSQL)
	if err != nil {
		return 0, fmt.Errorf("failed to prepare insert statement: %w", err)
	}
	defer hostStmt.Close()
	
	// Begin transaction
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	
	totalHosts := 0
	var totalParseTime, totalInsertTime time.Duration
	
	// Import all data
	for _, file := range files {
		// JSON parsing timing
		parseStart := time.Now()
		data, err := os.ReadFile(file.path)
		if err != nil {
			return 0, err
		}
		
		var hostSet herd.HostSet
		if useJsoniter {
			if err := jsonAPI.Unmarshal(data, &hostSet); err != nil {
				return 0, err
			}
		} else {
			if err := json.Unmarshal(data, &hostSet); err != nil {
				return 0, err
			}
		}
		parseTime := time.Since(parseStart)
		totalParseTime += parseTime
		
		provider := strings.TrimSuffix(filepath.Base(file.path), ".cache")
		
		// Insertion timing - OPTIMIZED VERSION
		insertStart := time.Now()
		
		// Pre-analyze which attributes actually exist in this dataset
		attributePresence := make(map[string]bool)
		for i := 0; i < hostSet.Len(); i++ {
			host := hostSet.Get(i)
			for attr := range host.Attributes {
				attributePresence[attr] = true
			}
		}
		
		// Create optimized INSERT statement with only columns that have data
		var presentAttributes []string
		var presentColumnNames []string
		for j, attr := range actualAttributes {
			if attributePresence[attr] {
				presentAttributes = append(presentAttributes, attr)
				presentColumnNames = append(presentColumnNames, actualColumnNames[j])
			}
		}
		
		fmt.Printf("  Optimizing: using %d/%d columns (%.1f%% reduction)\n", 
			len(presentAttributes), len(actualAttributes), 
			100.0*(float64(len(actualAttributes)-len(presentAttributes))/float64(len(actualAttributes))))
		
		// Build optimized INSERT statement
		optimizedInsertSQL := "INSERT INTO hosts (name, address, provider"
		optimizedValuesSQL := "VALUES (?, ?, ?"
		
		for _, columnName := range presentColumnNames {
			quotedColumnName := "`" + columnName + "`"
			optimizedInsertSQL += ", " + quotedColumnName
			optimizedValuesSQL += ", ?"
		}
		optimizedInsertSQL += ") " + optimizedValuesSQL + ")"
		
		// Prepare optimized statement
		optimizedStmt, err := db.Prepare(optimizedInsertSQL)
		if err != nil {
			return 0, fmt.Errorf("failed to prepare optimized insert: %w", err)
		}
		defer optimizedStmt.Close()
		
		// Check if bulk insert is requested (try it as an experiment)
		bulkInsert := false // Could be a flag, trying it automatically for now
		
		if bulkInsert && len(presentAttributes) < 50 {
			// OPTIMIZATION 2: Bulk INSERT for smaller column counts
			fmt.Printf("  Using bulk INSERT (experimental)\n")
			
			// Build bulk VALUES clause (SQLite supports up to 1000 rows per INSERT)
			batchSize := min(1000, hostSet.Len())
			
			for batchStart := 0; batchStart < hostSet.Len(); batchStart += batchSize {
				batchEnd := min(batchStart+batchSize, hostSet.Len())
				
				// Build bulk INSERT statement
				bulkSQL := optimizedInsertSQL[:len(optimizedInsertSQL)-1] // Remove closing paren
				bulkArgs := make([]interface{}, 0, (batchEnd-batchStart)*(3+len(presentAttributes)))
				
				for i := batchStart + 1; i < batchEnd; i++ {
					bulkSQL += ", (" + optimizedValuesSQL[7:] // Add more VALUE clauses
				}
				bulkSQL += ")"
				
				// Collect all values for this batch
				for i := batchStart; i < batchEnd; i++ {
					host := hostSet.Get(i)
					
					bulkArgs = append(bulkArgs, host.Name, host.Address, provider)
					
					for _, attr := range presentAttributes {
						if value, exists := host.Attributes[attr]; exists {
							bulkArgs = append(bulkArgs, extractStringValue(value))
						} else {
							bulkArgs = append(bulkArgs, nil)
						}
					}
				}
				
				if _, err := tx.Exec(bulkSQL, bulkArgs...); err != nil {
					return 0, fmt.Errorf("failed bulk insert: %w", err)
				}
				
				totalHosts += batchEnd - batchStart
			}
		} else {
			// OPTIMIZATION 1: Standard optimized insertion
			for i := 0; i < hostSet.Len(); i++ {
				host := hostSet.Get(i)
				
				// Build values slice only for present attributes
				values := make([]interface{}, 3+len(presentAttributes))
				values[0] = host.Name
				values[1] = host.Address
				values[2] = provider
				
				// Fill in only the attributes that are present in this dataset
				for j, attr := range presentAttributes {
					if value, exists := host.Attributes[attr]; exists {
						values[3+j] = extractStringValue(value)
					} else {
						values[3+j] = nil
					}
				}
				
				if _, err := tx.Stmt(optimizedStmt).Exec(values...); err != nil {
					return 0, fmt.Errorf("failed to insert host %s: %w", host.Name, err)
				}
				
				totalHosts++
			}
		}
		insertTime := time.Since(insertStart)
		totalInsertTime += insertTime
		
		fmt.Printf("- %s: parse %v, insert %v (%d hosts)\n", 
			provider, parseTime.Round(time.Millisecond), insertTime.Round(time.Millisecond), hostSet.Len())
	}
	
	// Commit transaction
	commitStart := time.Now()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	commitTime := time.Since(commitStart)
	
	_ = time.Since(importStart) // importTime unused in this function
	
	// PHASE 4: Create selective indexes
	var indexTime time.Duration
	if jsonIndex {
		fmt.Printf("\nPhase 4: Creating selective indexes...\n")
		indexStart := time.Now()
		
		// Index the most common attributes
		popularAttrs := []string{"app", "role", "app-role", "site", "region", "stamp"}
		
		for _, attr := range popularAttrs {
			columnName := strings.ReplaceAll(attr, "-", "_")
			columnName = strings.ReplaceAll(columnName, ".", "_")
			columnName = strings.ReplaceAll(columnName, ":", "_")
			
			// Check if this attribute exists in our actual schema
			exists := false
			for _, discovered := range actualAttributes {
				if discovered == attr {
					exists = true
					break
				}
			}
			
			if exists {
				indexSQL := fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s ON hosts(`%s`)", columnName, columnName)
				if _, err := db.Exec(indexSQL); err != nil {
					fmt.Printf("  Warning: failed to create index for %s: %v\n", attr, err)
				} else {
					fmt.Printf("  Created index for %s\n", attr)
				}
			}
		}
		
		indexTime = time.Since(indexStart)
	}
	
	totalTime := time.Since(totalStart)
	
	// Final summary
	fmt.Printf("\n=== FULL DENORMALIZATION COMPLETED ===\n")
	fmt.Printf("Timing breakdown:\n")
	fmt.Printf("- Discovery phase: %v\n", discoverTime.Round(time.Millisecond))
	fmt.Printf("- Schema creation: %v\n", schemaTime.Round(time.Millisecond))
	fmt.Printf("- Data parsing: %v\n", totalParseTime.Round(time.Millisecond))
	fmt.Printf("- Data insertion: %v\n", totalInsertTime.Round(time.Millisecond))
	fmt.Printf("- Transaction commit: %v\n", commitTime.Round(time.Millisecond))
	if indexTime > 0 {
		fmt.Printf("- Index creation: %v\n", indexTime.Round(time.Millisecond))
	}
	fmt.Printf("- Total time: %v\n", totalTime.Round(time.Millisecond))
	fmt.Printf("\nData summary:\n")
	fmt.Printf("- Total hosts: %d\n", totalHosts)
	fmt.Printf("- Unique attributes discovered: %d\n", len(attributeNames))
	fmt.Printf("- Actual columns created: %d\n", len(actualAttributes))
	fmt.Printf("- Storage: 100%% denormalized, 0%% JSON\n")
	fmt.Println()
	
	return totalHosts, nil
}

// Helper function to extract string value from any attribute type
func extractStringValue(value interface{}) string {
	switch v := value.(type) {
	case string:
		return v
	case []interface{}:
		if len(v) > 0 {
			if str, ok := v[0].(string); ok {
				return str
			}
		}
	case []string:
		if len(v) > 0 {
			return v[0]
		}
	case bool:
		if v {
			return "true"
		}
		return "false"
	case float64:
		return fmt.Sprintf("%.0f", v)
	case int:
		return fmt.Sprintf("%d", v)
	}
	return fmt.Sprintf("%v", value)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Helper function to extract string attributes safely
func getStringAttr(attrs map[string]interface{}, key string) *string {
	if value, ok := attrs[key]; ok {
		switch v := value.(type) {
		case string:
			return &v
		case []interface{}:
			if len(v) > 0 {
				if str, ok := v[0].(string); ok {
					return &str
				}
			}
		case []string:
			if len(v) > 0 {
				return &v[0]
			}
		}
	}
	return nil
}

func convertJSONToSQLite(files []cacheFile, dbPath string, debugMerge bool, debugHost string, useJsoniter bool, ftsIndex bool, jsonIndex bool) (int, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	
	// Create schema
	schema := `
	CREATE TABLE hosts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT UNIQUE NOT NULL,
		address TEXT,
		provider TEXT,
		attributes_json TEXT
	);
	
	CREATE TABLE attributes (
		host_id INTEGER,
		key TEXT,
		value TEXT,
		FOREIGN KEY(host_id) REFERENCES hosts(id)
	);
	
	CREATE INDEX idx_host_name ON hosts(name);
	CREATE INDEX idx_attr_key_value ON attributes(key, value);
	CREATE INDEX idx_attr_host_id ON attributes(host_id);
	`
	
	if _, err := db.Exec(schema); err != nil {
		return 0, fmt.Errorf("failed to create schema: %w", err)
	}
	
	// Prepare statements
	hostStmt, err := db.Prepare("INSERT INTO hosts (name, address, provider, attributes_json) VALUES (?, ?, ?, ?)")
	if err != nil {
		return 0, err
	}
	defer hostStmt.Close()
	
	updateHostStmt, err := db.Prepare("UPDATE hosts SET address = ?, provider = ?, attributes_json = ? WHERE name = ?")
	if err != nil {
		return 0, err
	}
	defer updateHostStmt.Close()
	
	selectHostStmt, err := db.Prepare("SELECT id FROM hosts WHERE name = ?")
	if err != nil {
		return 0, err
	}
	defer selectHostStmt.Close()
	
	attrStmt, err := db.Prepare("INSERT INTO attributes (host_id, key, value) VALUES (?, ?, ?)")
	if err != nil {
		return 0, err
	}
	defer attrStmt.Close()
	
	deleteAttrsStmt, err := db.Prepare("DELETE FROM attributes WHERE host_id = ?")
	if err != nil {
		return 0, err
	}
	defer deleteAttrsStmt.Close()
	
	// Begin transaction for better performance
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	
	totalHosts := 0
	mergedHosts := 0
	hostsPerFile := make(map[string]int)
	
	// Track all hosts and their merged attributes
	allHosts := make(map[string]*herd.Host)
	hostSources := make(map[string][]string) // Track which files each host came from
	
	// First pass: collect all hosts and merge attributes
	for _, file := range files {
		// Load hosts from JSON
		data, err := os.ReadFile(file.path)
		if err != nil {
			return 0, err
		}
		
		var hostSet herd.HostSet
		if useJsoniter {
			jsonAPI := jsoniter.ConfigCompatibleWithStandardLibrary
			if err := jsonAPI.Unmarshal(data, &hostSet); err != nil {
				return 0, err
			}
		} else {
			if err := json.Unmarshal(data, &hostSet); err != nil {
				return 0, err
			}
		}
		
		provider := strings.TrimSuffix(filepath.Base(file.path), ".cache")
		hostsPerFile[provider] = hostSet.Len()
		
		// Merge hosts
		for i := 0; i < hostSet.Len(); i++ {
			host := hostSet.Get(i)
			
			if existingHost, exists := allHosts[host.Name]; exists {
				// Track this merge
				hostSources[host.Name] = append(hostSources[host.Name], provider)
				
				if debugMerge && (debugHost == "" || debugHost == host.Name) {
					fmt.Printf("MERGE: Host %s found in %s (previously in %v)\n", host.Name, provider, hostSources[host.Name][:len(hostSources[host.Name])-1])
					fmt.Printf("  Existing attributes: %d\n", len(existingHost.Attributes))
					fmt.Printf("  New attributes: %d\n", len(host.Attributes))
				}
				
				// Merge attributes
				if existingHost.Attributes == nil {
					existingHost.Attributes = make(map[string]interface{})
				}
				
				attrsBefore := len(existingHost.Attributes)
				overwrittenAttrs := 0
				newAttrs := 0
				
				for k, v := range host.Attributes {
					if _, exists := existingHost.Attributes[k]; exists {
						overwrittenAttrs++
					} else {
						newAttrs++
					}
					existingHost.Attributes[k] = v
				}
				
				if debugMerge && (debugHost == "" || debugHost == host.Name) {
					fmt.Printf("  Merge result: %d attrs before, %d new, %d overwritten, %d total\n", 
						attrsBefore, newAttrs, overwrittenAttrs, len(existingHost.Attributes))
				}
				
				// Update address if empty
				if existingHost.Address == "" && host.Address != "" {
					existingHost.Address = host.Address
					if debugMerge && (debugHost == "" || debugHost == host.Name) {
						fmt.Printf("  Updated address: %s\n", host.Address)
					}
				}
				mergedHosts++
			} else {
				// Clone the host to avoid modifying the original
				newHost := &herd.Host{
					Name:       host.Name,
					Address:    host.Address,
					Attributes: make(map[string]interface{}),
				}
				for k, v := range host.Attributes {
					newHost.Attributes[k] = v
				}
				allHosts[host.Name] = newHost
				hostSources[host.Name] = []string{provider}
			}
		}
	}
	
	fmt.Printf("\nDebug: Import statistics\n")
	fmt.Printf("- Total unique hosts: %d\n", len(allHosts))
	fmt.Printf("- Hosts with merged data: %d\n", mergedHosts)
	for provider, count := range hostsPerFile {
		fmt.Printf("- Hosts in %s: %d\n", provider, count)
	}
	
	// Show hosts that appear in multiple sources
	multiSourceHosts := 0
	for hostName, sources := range hostSources {
		if len(sources) > 1 {
			multiSourceHosts++
			if debugMerge && multiSourceHosts <= 5 { // Show first 5 examples
				fmt.Printf("- Example: %s appears in %v\n", hostName, sources)
			}
		}
	}
	fmt.Printf("- Hosts appearing in multiple sources: %d\n", multiSourceHosts)
	fmt.Println()
	
	// Second pass: insert all merged hosts
	for _, host := range allHosts {
		// Serialize attributes as JSON for backup
		attrJSON, _ := json.Marshal(host.Attributes)
		
		// Try to insert host
		result, err := tx.Stmt(hostStmt).Exec(host.Name, host.Address, "merged", string(attrJSON))
		
		var hostID int64
		if err != nil {
			// Host already exists - shouldn't happen with clean DB, but handle it
			var existingID int64
			err = tx.Stmt(selectHostStmt).QueryRow(host.Name).Scan(&existingID)
			if err != nil {
				continue
			}
			
			// Update the existing host
			_, err = tx.Stmt(updateHostStmt).Exec(host.Address, "merged", string(attrJSON), host.Name)
			if err != nil {
				continue
			}
			
			// Delete existing attributes
			tx.Stmt(deleteAttrsStmt).Exec(existingID)
			
			hostID = existingID
		} else {
			hostID, err = result.LastInsertId()
			if err != nil {
				continue
			}
		}
		
		// Insert attributes
		for key, value := range host.Attributes {
			// Handle different value types
			switch v := value.(type) {
			case string:
				tx.Stmt(attrStmt).Exec(hostID, key, v)
			case []interface{}:
				for _, item := range v {
					if str, ok := item.(string); ok {
						tx.Stmt(attrStmt).Exec(hostID, key, str)
					}
				}
			case []string:
				for _, str := range v {
					tx.Stmt(attrStmt).Exec(hostID, key, str)
				}
			default:
				// Convert other types to string
				tx.Stmt(attrStmt).Exec(hostID, key, fmt.Sprintf("%v", v))
			}
		}
		
		totalHosts++
	}
	
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	
	return totalHosts, nil
}

func querySQLiteCountAll(db *sql.DB, filter string) (int, time.Duration, error) {
	start := time.Now()
	
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM hosts").Scan(&count)
	
	return count, time.Since(start), err
}

func querySQLiteFilter(db *sql.DB, filter string) (int, time.Duration, error) {
	if filter == "" {
		return querySQLiteCountAll(db, filter)
	}
	
	// Parse filter like "app-role=github-lowworker"
	parts := strings.SplitN(filter, "=", 2)
	if len(parts) != 2 {
		return querySQLiteCountAll(db, filter)
	}
	
	start := time.Now()
	
	query := `
		SELECT COUNT(DISTINCT h.id) 
		FROM hosts h 
		JOIN attributes a ON h.id = a.host_id 
		WHERE a.key = ? AND a.value = ?
	`
	
	var count int
	err := db.QueryRow(query, parts[0], parts[1]).Scan(&count)
	
	return count, time.Since(start), err
}

func querySQLiteBulkLoad(db *sql.DB, filter string) (int, time.Duration, error) {
	start := time.Now()
	
	var query string
	var args []interface{}
	
	if filter == "" {
		query = "SELECT name, address, attributes_json FROM hosts"
	} else {
		// Parse filter
		parts := strings.SplitN(filter, "=", 2)
		if len(parts) != 2 {
			query = "SELECT name, address, attributes_json FROM hosts"
		} else {
			query = `
				SELECT DISTINCT h.name, h.address, h.attributes_json 
				FROM hosts h 
				JOIN attributes a ON h.id = a.host_id 
				WHERE a.key = ? AND a.value = ?
			`
			args = []interface{}{parts[0], parts[1]}
		}
	}
	
	rows, err := db.Query(query, args...)
	if err != nil {
		return 0, time.Since(start), err
	}
	defer rows.Close()
	
	count := 0
	for rows.Next() {
		var name, address, attrJSON string
		if err := rows.Scan(&name, &address, &attrJSON); err != nil {
			continue
		}
		
		// Simulate actually using the data (unmarshal attributes)
		var attrs map[string]interface{}
		json.Unmarshal([]byte(attrJSON), &attrs)
		
		count++
	}
	
	return count, time.Since(start), rows.Err()
}

// JSON-based query functions

func querySQLiteJSONExtract(db *sql.DB, filter string) (int, time.Duration, error) {
	if filter == "" {
		return querySQLiteCountAll(db, filter)
	}
	
	// Parse filter - support multiple filters like "app=web,region=us-east"
	filters := strings.Split(filter, ",")
	if len(filters) > 1 {
		// For multiple filters, use the same logic as json_multi but with json_extract
		return querySQLiteJSONMulti(db, filter)
	}
	
	// Single filter handling
	parts := strings.SplitN(filter, "=", 2)
	if len(parts) != 2 {
		return querySQLiteCountAll(db, filter)
	}
	
	start := time.Now()
	
	var query string
	var args []interface{}
	
	// List of indexed attributes - use static queries for these
	indexedAttrs := map[string]bool{
		"app": true, "role": true, "app-role": true, 
		"site": true, "stamp": true, "region": true,
	}
	
	if indexedAttrs[parts[0]] {
		// Use static query for indexed attributes to match the index exactly
		query = fmt.Sprintf(`SELECT COUNT(*) FROM hosts WHERE json_extract(attributes_json, '$.%s') = ?`, parts[0])
		args = []interface{}{parts[1]}
	} else {
		// Dynamic query for non-indexed attributes
		query = `SELECT COUNT(*) FROM hosts WHERE json_extract(attributes_json, '$.' || ?) = ?`
		args = []interface{}{parts[0], parts[1]}
	}
	
	var count int
	err := db.QueryRow(query, args...).Scan(&count)
	
	return count, time.Since(start), err
}

func querySQLiteJSONArrow(db *sql.DB, filter string) (int, time.Duration, error) {
	if filter == "" {
		return querySQLiteCountAll(db, filter)
	}
	
	// Parse filter - support multiple filters like "app=web,region=us-east"
	filters := strings.Split(filter, ",")
	if len(filters) > 1 {
		// Build query with multiple WHERE conditions using ->> operator
		start := time.Now()
		
		var conditions []string
		var args []interface{}
		
		for _, f := range filters {
			parts := strings.SplitN(strings.TrimSpace(f), "=", 2)
			if len(parts) != 2 {
				continue
			}
			
			key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			conditions = append(conditions, "json_extract(attributes_json, '$.' || ?) = ?")
			args = append(args, key, value)
		}
		
		if len(conditions) == 0 {
			return querySQLiteCountAll(db, filter)
		}
		
		query := "SELECT COUNT(*) FROM hosts WHERE " + strings.Join(conditions, " AND ")
		
		var count int
		err := db.QueryRow(query, args...).Scan(&count)
		
		return count, time.Since(start), err
	}
	
	// Single filter handling
	parts := strings.SplitN(filter, "=", 2)
	if len(parts) != 2 {
		return querySQLiteCountAll(db, filter)
	}
	
	start := time.Now()
	
	// Using the ->> operator - simpler syntax than json_extract
	query := `
		SELECT COUNT(*) 
		FROM hosts 
		WHERE json_extract(attributes_json, '$.' || ?) = ?
	`
	
	var count int
	err := db.QueryRow(query, parts[0], parts[1]).Scan(&count)
	
	return count, time.Since(start), err
}

func querySQLiteJSONLike(db *sql.DB, filter string) (int, time.Duration, error) {
	if filter == "" {
		return querySQLiteCountAll(db, filter)
	}
	
	// Parse filter - support multiple filters like "app=web,region=us-east"
	filters := strings.Split(filter, ",")
	if len(filters) > 1 {
		// Build query with multiple LIKE conditions
		start := time.Now()
		
		var conditions []string
		var args []interface{}
		
		for _, f := range filters {
			parts := strings.SplitN(strings.TrimSpace(f), "=", 2)
			if len(parts) != 2 {
				continue
			}
			
			key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			conditions = append(conditions, `attributes_json LIKE '%"' || ? || '":"' || ? || '"%'`)
			args = append(args, key, value)
		}
		
		if len(conditions) == 0 {
			return querySQLiteCountAll(db, filter)
		}
		
		query := "SELECT COUNT(*) FROM hosts WHERE " + strings.Join(conditions, " AND ")
		
		var count int
		err := db.QueryRow(query, args...).Scan(&count)
		
		return count, time.Since(start), err
	}
	
	// Single filter handling
	parts := strings.SplitN(filter, "=", 2)
	if len(parts) != 2 {
		return querySQLiteCountAll(db, filter)
	}
	
	start := time.Now()
	
	// Using LIKE on the JSON text - less efficient but works on any SQLite version
	query := `
		SELECT COUNT(*) 
		FROM hosts 
		WHERE attributes_json LIKE '%"' || ? || '":"' || ? || '"%'
	`
	
	var count int
	err := db.QueryRow(query, parts[0], parts[1]).Scan(&count)
	
	return count, time.Since(start), err
}

func querySQLiteJSONBulkLoad(db *sql.DB, filter string) (int, time.Duration, error) {
	start := time.Now()
	
	var query string
	var args []interface{}
	
	if filter == "" {
		query = "SELECT name, address, attributes_json FROM hosts"
	} else {
		// Parse filter
		parts := strings.SplitN(filter, "=", 2)
		if len(parts) != 2 {
			query = "SELECT name, address, attributes_json FROM hosts"
		} else {
			query = `
				SELECT name, address, attributes_json 
				FROM hosts 
				WHERE json_extract(attributes_json, '$."' || ? || '"') = ?
			`
			args = []interface{}{parts[0], parts[1]}
		}
	}
	
	rows, err := db.Query(query, args...)
	if err != nil {
		return 0, time.Since(start), err
	}
	defer rows.Close()
	
	count := 0
	for rows.Next() {
		var name, address, attrJSON string
		if err := rows.Scan(&name, &address, &attrJSON); err != nil {
			continue
		}
		
		// Simulate actually using the data (unmarshal attributes)
		var attrs map[string]interface{}
		json.Unmarshal([]byte(attrJSON), &attrs)
		
		count++
	}
	
	return count, time.Since(start), rows.Err()
}

func querySQLiteFTS(db *sql.DB, filter string) (int, time.Duration, error) {
	if filter == "" {
		return querySQLiteCountAll(db, filter)
	}
	
	// Parse filter like "app-role=github-lowworker"
	parts := strings.SplitN(filter, "=", 2)
	if len(parts) != 2 {
		return querySQLiteCountAll(db, filter)
	}
	
	start := time.Now()
	
	// FTS search with post-filtering for accuracy
	query := `
		SELECT h.id
		FROM hosts_fts f
		JOIN hosts h ON h.id = f.rowid
		WHERE f.attributes_searchable MATCH ?
	`
	
	// Search for both terms
	searchTerm := `"` + parts[0] + `" AND "` + parts[1] + `"`
	
	rows, err := db.Query(query, searchTerm)
	if err != nil {
		return 0, time.Since(start), err
	}
	defer rows.Close()
	
	// Post-filter to ensure exact key-value match
	count := 0
	ftsMatches := 0
	for rows.Next() {
		var hostID int64
		if err := rows.Scan(&hostID); err != nil {
			continue
		}
		ftsMatches++
		
		// Check if this host actually has the exact key-value pair
		var attrValue sql.NullString
		checkQuery := `SELECT json_extract(attributes_json, '$.' || ?) FROM hosts WHERE id = ?`
		db.QueryRow(checkQuery, parts[0], hostID).Scan(&attrValue)
		
		if attrValue.Valid && attrValue.String == parts[1] {
			count++
		}
	}
	
	// Debug info about filtering (only show if there was filtering)
	if ftsMatches > count {
		fmt.Printf("  [FTS Debug: %d candidates → %d exact matches (filtered %d false positives)]\n", 
			ftsMatches, count, ftsMatches-count)
	}
	
	return count, time.Since(start), rows.Err()
}


func querySQLiteJSONMulti(db *sql.DB, filter string) (int, time.Duration, error) {
	if filter == "" {
		return querySQLiteCountAll(db, filter)
	}
	
	// Parse filter like "app=web,region=us-east,site=prod"
	filters := strings.Split(filter, ",")
	if len(filters) < 2 {
		// Fall back to single filter
		return querySQLiteJSONExtract(db, filter)
	}
	
	start := time.Now()
	
	// Build query with multiple WHERE conditions
	var conditions []string
	var args []interface{}
	
	indexedAttrs := map[string]bool{
		"app": true, "role": true, "app-role": true, 
		"site": true, "stamp": true, "region": true,
	}
	
	for _, f := range filters {
		parts := strings.SplitN(strings.TrimSpace(f), "=", 2)
		if len(parts) != 2 {
			continue
		}
		
		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		
		if indexedAttrs[key] {
			// Use static condition for indexed attributes
			conditions = append(conditions, fmt.Sprintf("json_extract(attributes_json, \"$.%s\") = ?", key))
		} else {
			// Dynamic condition for non-indexed attributes
			conditions = append(conditions, "json_extract(attributes_json, \"$.\" || ?) = ?")
			args = append(args, key)
		}
		args = append(args, value)
	}
	
	if len(conditions) == 0 {
		return querySQLiteCountAll(db, filter)
	}
	
	query := "SELECT COUNT(*) FROM hosts WHERE " + strings.Join(conditions, " AND ")
	
	var count int
	err := db.QueryRow(query, args...).Scan(&count)
	
	return count, time.Since(start), err
}


// NEW: Ultra-fast queries using denormalized columns
func querySQLiteDenormalized(db *sql.DB, filter string) (int, time.Duration, error) {
	if filter == "" {
		return querySQLiteCountAll(db, filter)
	}
	
	// Parse filter - support multiple filters like "app-role=github-dfs,region=iad"
	filters := strings.Split(filter, ",")
	
	start := time.Now()
	
	var conditions []string
	var args []interface{}
	
	// Map attribute names to denormalized column names
	attrToColumn := map[string]string{
		"app-role": "app_role",
		"region":   "region",
		"site":     "site", 
		"app":      "app",
		"role":     "role",
		"stamp":    "stamp",
	}
	
	for _, f := range filters {
		parts := strings.SplitN(strings.TrimSpace(f), "=", 2)
		if len(parts) != 2 {
			continue
		}
		
		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		
		if column, ok := attrToColumn[key]; ok {
			// Use fast denormalized column
			conditions = append(conditions, column + " = ?")
			args = append(args, value)
		} else {
			// Fall back to JSON extraction for non-denormalized attributes
			conditions = append(conditions, "json_extract(attributes_json, '$.' || ?) = ?")
			args = append(args, key, value)
		}
	}
	
	if len(conditions) == 0 {
		return querySQLiteCountAll(db, filter)
	}
	
	query := "SELECT COUNT(*) FROM hosts WHERE " + strings.Join(conditions, " AND ")
	
	var count int
	err := db.QueryRow(query, args...).Scan(&count)
	
	return count, time.Since(start), err
}

// ULTIMATE: Fully denormalized queries - no JSON at all\!
func querySQLiteFullDenorm(db *sql.DB, filter string) (int, time.Duration, error) {
	if filter == "" {
		return querySQLiteCountAll(db, filter)
	}
	
	// Parse filter - support multiple filters like "app-role=github-dfs,region=iad"
	filters := strings.Split(filter, ",")
	
	start := time.Now()
	
	var conditions []string
	var args []interface{}
	
	for _, f := range filters {
		parts := strings.SplitN(strings.TrimSpace(f), "=", 2)
		if len(parts) != 2 {
			continue
		}
		
		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		
		// Convert attribute name to column name (same logic as in conversion)
		columnName := strings.ReplaceAll(key, "-", "_")
		columnName = strings.ReplaceAll(columnName, ".", "_")
		columnName = strings.ReplaceAll(columnName, ":", "_")
		columnName = "`" + columnName + "`" // Quote for safety
		
		conditions = append(conditions, columnName + " = ?")
		args = append(args, value)
	}
	
	if len(conditions) == 0 {
		return querySQLiteCountAll(db, filter)
	}
	
	query := "SELECT COUNT(*) FROM hosts WHERE " + strings.Join(conditions, " AND ")
	
	var count int
	err := db.QueryRow(query, args...).Scan(&count)
	
	return count, time.Since(start), err
}
