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
		return runSQLiteBenchmark(files, filter, parseOnly, sqliteOnly)
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

func runSQLiteBenchmark(files []cacheFile, filter string, parseOnly bool, sqliteOnly bool) error {
	dbPath := "benchmark_cache.db"
	
	// Clean up any existing database
	os.Remove(dbPath)
	defer os.Remove(dbPath)
	
	if !sqliteOnly {
		fmt.Println("=== SQLite Conversion Benchmark ===")
		
		// Benchmark conversion from JSON to SQLite
		start := time.Now()
		totalHosts, err := convertJSONToSQLite(files, dbPath)
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
		// For sqlite-only mode, assume database exists or create a quick one
		if _, err := os.Stat(dbPath); os.IsNotExist(err) {
			fmt.Println("=== SQLite Conversion (for sqlite-only mode) ===")
			start := time.Now()
			totalHosts, err := convertJSONToSQLite(files, dbPath)
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
	if filter != "" {
		fmt.Printf("DEBUG: Looking for filter '%s'\n", filter)
		db, err := sql.Open("sqlite", dbPath)
		if err == nil {
			parts := strings.SplitN(filter, "=", 2)
			if len(parts) == 2 {
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
			db.Close()
		}
		fmt.Println()
	}
	
	// Test different query types
	queries := []struct {
		name        string
		description string
		testFunc    func(*sql.DB, string) (int, time.Duration, error)
	}{
		{"count_all", "Count all hosts", querySQLiteCountAll},
		{"filter_attr", "Filter by attribute", querySQLiteFilter},
		{"bulk_load", "Load all hosts", querySQLiteBulkLoad},
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

func convertJSONToSQLite(files []cacheFile, dbPath string) (int, error) {
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
	
	attrStmt, err := db.Prepare("INSERT INTO attributes (host_id, key, value) VALUES (?, ?, ?)")
	if err != nil {
		return 0, err
	}
	defer attrStmt.Close()
	
	// Begin transaction for better performance
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	
	totalHosts := 0
	
	for _, file := range files {
		// Load hosts from JSON
		data, err := os.ReadFile(file.path)
		if err != nil {
			return 0, err
		}
		
		var hostSet herd.HostSet
		if err := json.Unmarshal(data, &hostSet); err != nil {
			return 0, err
		}
		
		provider := strings.TrimSuffix(filepath.Base(file.path), ".cache")
		
		// Insert each host
		for i := 0; i < hostSet.Len(); i++ {
			host := hostSet.Get(i)
			
			// Serialize attributes as JSON for backup
			attrJSON, _ := json.Marshal(host.Attributes)
			
			// Insert host
			result, err := tx.Stmt(hostStmt).Exec(host.Name, host.Address, provider, string(attrJSON))
			if err != nil {
				continue // Skip duplicates
			}
			
			hostID, err := result.LastInsertId()
			if err != nil {
				continue
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