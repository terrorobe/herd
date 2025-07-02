package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/seveas/herd"
	"github.com/spf13/cobra"

	jsoniter "github.com/json-iterator/go"
	jsonv2 "github.com/go-json-experiment/json"
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

const toolVersion = "v2.1.0-streamlined"

var rootCmd = &cobra.Command{
	Use:   "herd-cache-bench [flags] [cache-dir]",
	Short: "Benchmark JSON vs SQLite performance for Herd cache files",
	Long: `Streamlined benchmark tool comparing JSON parsing vs SQLite with denormalized columns.
Default mode uses fast ephemeral SQLite with hybrid denormalization.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runBenchmark,
}

func init() {
	rootCmd.Flags().StringP("filter", "f", "", "Filter hosts by attribute (e.g., 'app-role=github-lowworker')")
	rootCmd.Flags().BoolP("sqlite-only", "", false, "Only test SQLite (skip JSON parsing benchmark)")
	rootCmd.Flags().BoolP("use-jsoniter", "", false, "Use jsoniter instead of stdlib for JSON parsing")
	rootCmd.Flags().BoolP("use-jsonv2", "", false, "Use experimental json/v2 for JSON parsing")
	rootCmd.Flags().BoolP("force-rebuild", "", false, "Force database rebuild (delete existing database)")
	rootCmd.Flags().BoolP("fts-index", "", false, "Create FTS index for experimental comparison")
	rootCmd.Flags().BoolP("json-index", "", false, "Create JSON indexes for comparison with denormalized columns")
	rootCmd.Flags().BoolP("dump-sql", "", false, "Dump SQL queries during execution")
	rootCmd.Flags().BoolP("multi-provider", "", false, "Use UNION query for multi-provider attribute matching")
	rootCmd.Flags().BoolP("merge-attributes", "", false, "Merge all provider attributes per host during import")
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
	sqliteOnly, _ := cmd.Flags().GetBool("sqlite-only")
	useJsoniter, _ := cmd.Flags().GetBool("use-jsoniter")
	useJsonv2, _ := cmd.Flags().GetBool("use-jsonv2")
	forceRebuild, _ := cmd.Flags().GetBool("force-rebuild")
	ftsIndex, _ := cmd.Flags().GetBool("fts-index")
	jsonIndex, _ := cmd.Flags().GetBool("json-index")
	dumpSQL, _ := cmd.Flags().GetBool("dump-sql")
	multiProvider, _ := cmd.Flags().GetBool("multi-provider")
	mergeAttributes, _ := cmd.Flags().GetBool("merge-attributes")

	fmt.Printf("herd-cache-bench %s\n", toolVersion)
	fmt.Printf("Found %d cache files in %s\n", len(files), cacheDir)
	fmt.Printf("Total size: %.2f MB\n", float64(totalSize(files))/(1024*1024))
	
	if filter != "" {
		fmt.Printf("Filter: %s\n", filter)
	}

	if mergeAttributes {
		fmt.Printf("Mode: Merged attributes (like Herd)\n")
	}
	
	if sqliteOnly {
		fmt.Printf("Mode: SQLite-only (fast ephemeral with hybrid denormalization)\n")
		return runSQLiteOnlyBenchmark(files, filter, useJsoniter, useJsonv2, forceRebuild, ftsIndex, jsonIndex, dumpSQL, multiProvider, mergeAttributes)
	} else {
		fmt.Printf("Mode: JSON vs SQLite comparison\n")
		return runFullBenchmark(files, filter, useJsoniter, useJsonv2, forceRebuild, ftsIndex, jsonIndex, dumpSQL, multiProvider, mergeAttributes)
	}
}

func runSQLiteOnlyBenchmark(files []cacheFile, filter string, useJsoniter bool, useJsonv2 bool, forceRebuild bool, ftsIndex bool, jsonIndex bool, dumpSQL bool, multiProvider bool, mergeAttributes bool) error {
	cacheDir := filepath.Join(os.Getenv("HOME"), ".cache", "herd")
	dbPath := filepath.Join(cacheDir, "benchmark_cache.db")
	
	fmt.Printf("Database will be stored at: %s\n", dbPath)
	
	// Check if we need to create/rebuild the database
	needsRebuild := forceRebuild || func() bool {
		if _, err := os.Stat(dbPath); os.IsNotExist(err) {
			return true // File doesn't exist, need to import
		}
		
		// Check if the existing database has the right indexes for our test mode
		return !hasCorrectIndexes(dbPath, ftsIndex, jsonIndex)
	}()
	
	if needsRebuild {
		if forceRebuild {
			fmt.Printf("Force rebuilding database...\n")
			os.Remove(dbPath) // Delete existing database
		} else {
			fmt.Printf("Database missing required indexes, rebuilding...\n")
		}
		fmt.Println("\n=== SQLite Import (Ephemeral Mode) ===")
		
		totalHosts, dataImportTime, readTime, writeTime, indexTime, err := convertToSQLite(files, dbPath, useJsoniter, useJsonv2, ftsIndex, jsonIndex, mergeAttributes)
		if err != nil {
			return fmt.Errorf("SQLite conversion failed: %w", err)
		}
		
		conversionTime := dataImportTime + indexTime
		
		// Get database file size
		dbInfo, err := os.Stat(dbPath)
		if err != nil {
			return fmt.Errorf("failed to get database size: %w", err)
		}
		
		fmt.Printf("Import completed:\n")
		fmt.Printf("- File reading + JSON parsing: %v\n", readTime.Round(time.Millisecond))
		fmt.Printf("- Database writes: %v\n", writeTime.Round(time.Millisecond))
		fmt.Printf("- Data import total: %v\n", dataImportTime.Round(time.Millisecond))
		fmt.Printf("- Index creation: %v\n", indexTime.Round(time.Millisecond))
		fmt.Printf("- Total time: %v\n", conversionTime.Round(time.Millisecond))
		fmt.Printf("- Hosts imported: %d\n", totalHosts)
		fmt.Printf("- Database size: %.2f MB\n", float64(dbInfo.Size())/(1024*1024))
		fmt.Printf("- Compression ratio: %.1fx\n\n", float64(totalSize(files))/float64(dbInfo.Size()))
	} else {
		fmt.Println("Using existing SQLite database\n")
	}
	
	return runSQLiteQueries(dbPath, filter, ftsIndex, jsonIndex, dumpSQL, multiProvider, mergeAttributes)
}

func runFullBenchmark(files []cacheFile, filter string, useJsoniter bool, useJsonv2 bool, forceRebuild bool, ftsIndex bool, jsonIndex bool, dumpSQL bool, multiProvider bool, mergeAttributes bool) error {
	fmt.Println("\n=== JSON Parsing Benchmark ===")
	
	// Test JSON parsing first
	parsers := []string{"stdlib"}
	if useJsoniter {
		parsers = append(parsers, "jsoniter")
	}
	if useJsonv2 {
		parsers = append(parsers, "jsonv2")
	}
	
	fmt.Printf("System info: %s, %d cores\n", runtime.GOARCH, runtime.NumCPU())
	
	var jsonTime time.Duration
	
	for _, parser := range parsers {
		result := benchmarkParser(parser, files, filter)
		fmt.Printf("%-12s: %12s (%d hosts)\n", parser, result.duration.Round(time.Millisecond), result.hosts)
		// Track the fastest parser's time
		if (parser == "jsonv2" && useJsonv2) || 
		   (parser == "jsoniter" && useJsoniter && !useJsonv2) || 
		   (parser == "stdlib" && !useJsoniter && !useJsonv2) {
			jsonTime = result.duration
		}
	}
	
	// Now test SQLite
	fmt.Println("\n=== SQLite Comparison ===")
	cacheDir := filepath.Join(os.Getenv("HOME"), ".cache", "herd")
	dbPath := filepath.Join(cacheDir, "benchmark_cache.db")
	
	// Force rebuild for fair comparison
	if forceRebuild {
		os.Remove(dbPath)
	}
	
	totalHosts, dataImportTime, readTime, writeTime, indexTime, err := convertToSQLite(files, dbPath, useJsoniter, useJsonv2, ftsIndex, jsonIndex, mergeAttributes)
	if err != nil {
		return err
	}
	sqliteImportTime := dataImportTime + indexTime
	
	fmt.Printf("SQLite file reading: %v\n", readTime.Round(time.Millisecond))
	fmt.Printf("SQLite database writes: %v\n", writeTime.Round(time.Millisecond))
	fmt.Printf("SQLite data import: %v\n", dataImportTime.Round(time.Millisecond))
	fmt.Printf("SQLite index creation: %v\n", indexTime.Round(time.Millisecond))
	fmt.Printf("SQLite total: %v (%d hosts)\n", sqliteImportTime.Round(time.Millisecond), totalHosts)
	
	// Quick query test
	start := time.Now()
	count, err := querySQLiteDenormalized(dbPath, filter, dumpSQL)
	if err != nil {
		return err
	}
	sqliteQueryTime := time.Since(start)
	
	fmt.Printf("SQLite query:  %v (%d hosts)\n", sqliteQueryTime.Round(time.Microsecond), count)
	
	// Show comparison
	fmt.Println("\n=== Performance Summary ===")
	fmt.Printf("JSON parsing:  %v\n", jsonTime.Round(time.Millisecond))
	fmt.Printf("SQLite query:  %v\n", sqliteQueryTime.Round(time.Microsecond))
	if sqliteQueryTime > 0 {
		speedup := float64(jsonTime) / float64(sqliteQueryTime)
		fmt.Printf("Speedup:       %.0fx faster with SQLite\n", speedup)
	}
	
	return nil
}

func convertToSQLite(files []cacheFile, dbPath string, useJsoniter bool, useJsonv2 bool, ftsIndex bool, jsonIndex bool, mergeAttributes bool) (int, time.Duration, time.Duration, time.Duration, time.Duration, error) {
	dataImportStart := time.Now()
	
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	defer db.Close()
	
	// Fast ephemeral mode (always on)
	fastMode := `
	PRAGMA journal_mode = MEMORY;
	PRAGMA synchronous = OFF;
	PRAGMA cache_size = 100000;
	PRAGMA temp_store = MEMORY;
	PRAGMA foreign_keys = OFF;
	
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
		return 0, 0, 0, 0, 0, fmt.Errorf("failed to setup database: %w", err)
	}
	
	// Prepare statement
	hostStmt, err := db.Prepare("INSERT INTO hosts (name, address, provider, attributes_json, app_role, region, site, app, role, stamp) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	defer hostStmt.Close()
	
	// Begin transaction
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	defer tx.Rollback()
	
	totalHosts := 0
	var totalReadTime, totalWriteTime time.Duration
	
	// Setup JSON parser
	var jsonAPI jsoniter.API
	if useJsoniter {
		jsonAPI = jsoniter.ConfigCompatibleWithStandardLibrary
		fmt.Printf("Using jsoniter for JSON parsing\n")
	} else if useJsonv2 {
		fmt.Printf("Using json/v2 for JSON parsing\n")
	} else {
		fmt.Printf("Using stdlib for JSON parsing\n")
	}
	
	if mergeAttributes {
		return convertToSQLiteMerged(files, db, hostStmt, tx, useJsoniter, useJsonv2, jsonAPI, totalReadTime, totalWriteTime, dataImportStart, ftsIndex, jsonIndex)
	}
	
	// Process each file (original approach)
	for _, file := range files {
		// Time file reading and parsing
		readStart := time.Now()
		data, err := os.ReadFile(file.path)
		if err != nil {
			return 0, 0, 0, 0, 0, err
		}
		
		var hostSet herd.HostSet
		if useJsonv2 {
			if err := jsonv2.Unmarshal(data, &hostSet); err != nil {
				return 0, 0, 0, 0, 0, err
			}
		} else if useJsoniter {
			if err := jsonAPI.Unmarshal(data, &hostSet); err != nil {
				return 0, 0, 0, 0, 0, err
			}
		} else {
			if err := json.Unmarshal(data, &hostSet); err != nil {
				return 0, 0, 0, 0, 0, err
			}
		}
		readTime := time.Since(readStart)
		totalReadTime += readTime
		
		provider := strings.TrimSuffix(filepath.Base(file.path), ".cache")
		
		// Time database writes
		writeStart := time.Now()
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
				return 0, 0, 0, 0, 0, fmt.Errorf("failed to insert host %s: %w", host.Name, err)
			}
			
			totalHosts++
		}
		writeTime := time.Since(writeStart)
		totalWriteTime += writeTime
		
		fmt.Printf("- %s: %d hosts (read: %v, write: %v)\n", provider, hostSet.Len(), 
			readTime.Round(time.Millisecond), writeTime.Round(time.Millisecond))
	}
	
	if err := tx.Commit(); err != nil {
		return 0, 0, 0, 0, 0, err
	}
	
	dataImportTime := time.Since(dataImportStart)
	
	// Create indexes separately
	indexTime, err := createIndexes(db, jsonIndex, ftsIndex)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("failed to create indexes: %w", err)
	}
	
	return totalHosts, dataImportTime, totalReadTime, totalWriteTime, indexTime, nil
}

func createIndexes(db *sql.DB, jsonIndex bool, ftsIndex bool) (time.Duration, error) {
	if !jsonIndex && !ftsIndex {
		// Still create basic denormalized indexes
		return createDenormalizedIndexes(db)
	}
	
	indexStart := time.Now()
	fmt.Printf("Creating indexes...\n")
	
	var totalIndexTime time.Duration
	
	// Always create denormalized indexes first
	denormStart := time.Now()
	denormIndexes := []string{
		"CREATE INDEX IF NOT EXISTS idx_app_role ON hosts(app_role)",
		"CREATE INDEX IF NOT EXISTS idx_region ON hosts(region)",
		"CREATE INDEX IF NOT EXISTS idx_site ON hosts(site)",
		"CREATE INDEX IF NOT EXISTS idx_app_role_region ON hosts(app_role, region)",
	}
	
	for _, indexSQL := range denormIndexes {
		if _, err := db.Exec(indexSQL); err != nil {
			fmt.Printf("Warning: failed to create denormalized index: %v\n", err)
		}
	}
	denormTime := time.Since(denormStart)
	totalIndexTime += denormTime
	fmt.Printf("- Denormalized indexes: %v\n", denormTime.Round(time.Millisecond))
	
	if jsonIndex {
		jsonStart := time.Now()
		// JSON indexes for comparison
		jsonIndexes := []string{
			"CREATE INDEX IF NOT EXISTS idx_json_app_role ON hosts(json_extract(attributes_json, '$.app-role'))",
			"CREATE INDEX IF NOT EXISTS idx_json_region ON hosts(json_extract(attributes_json, '$.region'))",
			"CREATE INDEX IF NOT EXISTS idx_json_site ON hosts(json_extract(attributes_json, '$.site'))",
		}
		
		for _, indexSQL := range jsonIndexes {
			if _, err := db.Exec(indexSQL); err != nil {
				fmt.Printf("Warning: failed to create JSON index: %v\n", err)
			}
		}
		jsonTime := time.Since(jsonStart)
		totalIndexTime += jsonTime
		fmt.Printf("- JSON indexes: %v\n", jsonTime.Round(time.Millisecond))
	}
	
	if ftsIndex {
		ftsStart := time.Now()
		// FTS for experimentation
		ftsSQL := `
		CREATE VIRTUAL TABLE IF NOT EXISTS hosts_fts USING fts5(
			name,
			attributes_searchable,
			content='hosts',
			content_rowid='id'
		)`
		if _, err := db.Exec(ftsSQL); err != nil {
			fmt.Printf("Warning: failed to create FTS table: %v\n", err)
		} else {
			// Populate FTS with properly formatted key=value pairs
			populateSQL := `
			INSERT INTO hosts_fts(rowid, name, attributes_searchable)
			SELECT id, name, 
				   REPLACE(REPLACE(REPLACE(REPLACE(
					   attributes_json, 
					   '":', '='), 
					   '","', ' '), 
					   '{', ''), 
					   '}', '') || ' ' ||
				   REPLACE(REPLACE(
					   attributes_json,
					   '"', ''),
					   ',', ' ')
			FROM hosts`
			if _, err := db.Exec(populateSQL); err != nil {
				fmt.Printf("Warning: failed to populate FTS: %v\n", err)
			}
		}
		ftsTime := time.Since(ftsStart)
		totalIndexTime += ftsTime
		fmt.Printf("- FTS index + population: %v\n", ftsTime.Round(time.Millisecond))
	}
	
	return time.Since(indexStart), nil
}

func createDenormalizedIndexes(db *sql.DB) (time.Duration, error) {
	start := time.Now()
	
	denormIndexes := []string{
		"CREATE INDEX IF NOT EXISTS idx_app_role ON hosts(app_role)",
		"CREATE INDEX IF NOT EXISTS idx_region ON hosts(region)",
		"CREATE INDEX IF NOT EXISTS idx_site ON hosts(site)",
		"CREATE INDEX IF NOT EXISTS idx_app_role_region ON hosts(app_role, region)",
	}
	
	for _, indexSQL := range denormIndexes {
		if _, err := db.Exec(indexSQL); err != nil {
			return time.Since(start), err
		}
	}
	
	return time.Since(start), nil
}

func runSQLiteQueries(dbPath string, filter string, ftsIndex bool, jsonIndex bool, dumpSQL bool, multiProvider bool, mergeAttributes bool) error {
	fmt.Println("=== SQLite Query Benchmark ===")
	
	// Define queries to test
	var queries []struct {
		name        string
		description string
		testFunc    func(string, string, bool) (int, time.Duration, error)
	}
	
	queries = append(queries, 
		struct {
			name        string
			description string
			testFunc    func(string, string, bool) (int, time.Duration, error)
		}{"count_all", "Count all hosts", queryCountAll})
	
	if mergeAttributes {
		queries = append(queries,
			struct {
				name        string
				description string
				testFunc    func(string, string, bool) (int, time.Duration, error)
			}{"merged_filter", "Merged attributes (like Herd)", queryMerged})
	} else {
		queries = append(queries,
			struct {
				name        string
				description string
				testFunc    func(string, string, bool) (int, time.Duration, error)
			}{"denorm_filter", "Denormalized columns (FAST!)", queryDenormalized})
		
		if multiProvider && filter != "" && strings.Contains(filter, ",") {
			queries = append(queries,
				struct {
					name        string
					description string
					testFunc    func(string, string, bool) (int, time.Duration, error)
				}{"multi_provider", "Multi-provider UNION (cross-provider attributes)", queryMultiProvider})
		}
	}
	
	if jsonIndex {
		queries = append(queries,
			struct {
				name        string
				description string
				testFunc    func(string, string, bool) (int, time.Duration, error)
			}{"json_extract", "JSON indexes (comparison)", queryJSONExtract})
	}
	
	if ftsIndex {
		queries = append(queries,
			struct {
				name        string
				description string
				testFunc    func(string, string, bool) (int, time.Duration, error)
			}{"fts_search", "Full-text search (experimental)", queryFTS})
	}
	
	// Run each query 3 times
	for _, query := range queries {
		fmt.Printf("Testing %s (%s):\n", query.name, query.description)
		
		var totalTime time.Duration
		var results int
		validRuns := 0
		
		for i := 0; i < 3; i++ {
			count, duration, err := query.testFunc(dbPath, filter, dumpSQL)
			if err != nil {
				fmt.Printf("  Iteration %d: ERROR: %v\n", i+1, err)
			} else {
				fmt.Printf("  Iteration %d: %v (%d results)\n", i+1, duration.Round(time.Microsecond), count)
				totalTime += duration
				results = count
				validRuns++
			}
		}
		
		if validRuns > 0 {
			avgTime := totalTime / time.Duration(validRuns)
			fmt.Printf("  Average: %v (%d results)\n", avgTime.Round(time.Microsecond), results)
		}
		fmt.Println()
	}
	
	return nil
}

// Helper functions
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

func benchmarkParser(parser string, files []cacheFile, filter string) benchResult {
	start := time.Now()
	totalHosts := 0
	
	for _, f := range files {
		hosts, err := parseFile(parser, f.path, filter)
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

func parseFile(parser string, path string, filter string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	
	var hosts herd.HostSet
	
	switch parser {
	case "stdlib":
		if err := json.Unmarshal(data, &hosts); err != nil {
			return 0, err
		}
	case "jsoniter":
		jsonAPI := jsoniter.ConfigCompatibleWithStandardLibrary
		if err := jsonAPI.Unmarshal(data, &hosts); err != nil {
			return 0, err
		}
	case "jsonv2":
		if err := jsonv2.Unmarshal(data, &hosts); err != nil {
			return 0, err
		}
	default:
		return 0, fmt.Errorf("unknown parser: %s", parser)
	}
	
	if filter == "" {
		return hosts.Len(), nil
	}
	
	return filterHosts(&hosts, filter), nil
}

func filterHosts(hosts *herd.HostSet, filter string) int {
	if filter == "" {
		return hosts.Len()
	}
	
	// Parse filter like "app-role=github-lowworker"
	parts := strings.SplitN(filter, "=", 2)
	if len(parts) != 2 {
		return hosts.Len()
	}
	
	key, value := parts[0], parts[1]
	count := 0
	
	for i := 0; i < hosts.Len(); i++ {
		host := hosts.Get(i)
		if attrs, ok := host.Attributes[key]; ok {
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

// Query functions
func queryCountAll(dbPath string, filter string, dumpSQL bool) (int, time.Duration, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	
	query := "SELECT COUNT(*) FROM hosts"
	if dumpSQL {
		fmt.Printf("  SQL: %s\n", query)
	}
	
	start := time.Now()
	var count int
	err = db.QueryRow(query).Scan(&count)
	return count, time.Since(start), err
}

func queryDenormalized(dbPath string, filter string, dumpSQL bool) (int, time.Duration, error) {
	if filter == "" {
		return queryCountAll(dbPath, filter, dumpSQL)
	}
	
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	
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
			if value == "true" || value == "false" {
				// Handle boolean values
				conditions = append(conditions, fmt.Sprintf("json_extract(attributes_json, '$.' || ?) = %s", value))
				args = append(args, key)
			} else {
				// Handle string values
				conditions = append(conditions, "json_extract(attributes_json, '$.' || ?) = ?")
				args = append(args, key, value)
			}
		}
	}
	
	if len(conditions) == 0 {
		return queryCountAll(dbPath, filter, dumpSQL)
	}
	
	query := "SELECT COUNT(*) FROM hosts WHERE " + strings.Join(conditions, " AND ")
	
	if dumpSQL {
		fmt.Printf("  SQL: %s\n", query)
		fmt.Printf("  Args: %v\n", args)
	}
	
	var count int
	err = db.QueryRow(query, args...).Scan(&count)
	
	return count, time.Since(start), err
}

func queryJSONExtract(dbPath string, filter string, dumpSQL bool) (int, time.Duration, error) {
	if filter == "" {
		return queryCountAll(dbPath, filter, dumpSQL)
	}
	
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	
	// Parse filter - support multiple filters
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
		if value == "true" || value == "false" {
			// Handle boolean values
			conditions = append(conditions, fmt.Sprintf("json_extract(attributes_json, '$.' || ?) = %s", value))
			args = append(args, key)
		} else {
			// Handle string values
			conditions = append(conditions, "json_extract(attributes_json, '$.' || ?) = ?")
			args = append(args, key, value)
		}
	}
	
	if len(conditions) == 0 {
		return queryCountAll(dbPath, filter, dumpSQL)
	}
	
	query := "SELECT COUNT(*) FROM hosts WHERE " + strings.Join(conditions, " AND ")
	
	if dumpSQL {
		fmt.Printf("  SQL: %s\n", query)
		fmt.Printf("  Args: %v\n", args)
	}
	
	var count int
	err = db.QueryRow(query, args...).Scan(&count)
	
	return count, time.Since(start), err
}

func queryFTS(dbPath string, filter string, dumpSQL bool) (int, time.Duration, error) {
	if filter == "" {
		return queryCountAll(dbPath, filter, dumpSQL)
	}
	
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	
	// Parse multiple filters like "app-role=github-dfs,site=va3-iad"
	filters := strings.Split(filter, ",")
	
	start := time.Now()
	
	// Build FTS search terms for all key-value pairs
	var searchTerms []string
	var filterPairs []struct{ key, value string }
	
	for _, f := range filters {
		parts := strings.SplitN(strings.TrimSpace(f), "=", 2)
		if len(parts) != 2 {
			continue
		}
		
		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		filterPairs = append(filterPairs, struct{ key, value string }{key, value})
		
		// Search for key=value pattern in the FTS text
		searchTerms = append(searchTerms, `"`+key+`=`+value+`"`)
	}
	
	if len(searchTerms) == 0 {
		return queryCountAll(dbPath, filter, dumpSQL)
	}
	
	// FTS search with all terms
	ftsQuery := `
		SELECT h.id, h.attributes_json
		FROM hosts_fts f
		JOIN hosts h ON h.id = f.rowid
		WHERE f.attributes_searchable MATCH ?
	`
	
	// Combine search terms with AND
	searchPattern := strings.Join(searchTerms, " AND ")
	
	if dumpSQL {
		fmt.Printf("  SQL: %s\n", strings.TrimSpace(ftsQuery))
		fmt.Printf("  Args: [%s]\n", searchPattern)
	}
	
	rows, err := db.Query(ftsQuery, searchPattern)
	if err != nil {
		return 0, time.Since(start), err
	}
	defer rows.Close()
	
	// Post-filter to ensure exact key-value matches for all filters
	count := 0
	ftsMatches := 0
	
	for rows.Next() {
		var hostID int64
		var attrJSON string
		if err := rows.Scan(&hostID, &attrJSON); err != nil {
			continue
		}
		ftsMatches++
		
		// Parse the JSON attributes
		var attrs map[string]interface{}
		if err := json.Unmarshal([]byte(attrJSON), &attrs); err != nil {
			continue
		}
		
		// Check if ALL filter conditions match
		allMatch := true
		for _, pair := range filterPairs {
			if value, exists := attrs[pair.key]; exists {
				switch v := value.(type) {
				case string:
					if v != pair.value {
						allMatch = false
						break
					}
				case []interface{}:
					found := false
					for _, item := range v {
						if str, ok := item.(string); ok && str == pair.value {
							found = true
							break
						}
					}
					if !found {
						allMatch = false
						break
					}
				case []string:
					found := false
					for _, str := range v {
						if str == pair.value {
							found = true
							break
						}
					}
					if !found {
						allMatch = false
						break
					}
				default:
					allMatch = false
					break
				}
			} else {
				allMatch = false
				break
			}
		}
		
		if allMatch {
			count++
		}
	}
	
	// Debug info about FTS effectiveness
	if ftsMatches != count {
		fmt.Printf("  [FTS Debug: %d candidates → %d exact matches (filtered %d false positives)]\n", 
			ftsMatches, count, ftsMatches-count)
	}
	
	return count, time.Since(start), rows.Err()
}

func convertToSQLiteMerged(files []cacheFile, db *sql.DB, hostStmt *sql.Stmt, tx *sql.Tx, useJsoniter bool, useJsonv2 bool, jsonAPI jsoniter.API, totalReadTime time.Duration, totalWriteTime time.Duration, dataImportStart time.Time, ftsIndex bool, jsonIndex bool) (int, time.Duration, time.Duration, time.Duration, time.Duration, error) {
	// First pass: collect all hosts by name and merge attributes
	mergedHosts := make(map[string]*MergedHost)
	
	for _, file := range files {
		readStart := time.Now()
		data, err := os.ReadFile(file.path)
		if err != nil {
			return 0, 0, 0, 0, 0, err
		}
		
		var hostSet herd.HostSet
		if useJsonv2 {
			if err := jsonv2.Unmarshal(data, &hostSet); err != nil {
				return 0, 0, 0, 0, 0, err
			}
		} else if useJsoniter {
			if err := jsonAPI.Unmarshal(data, &hostSet); err != nil {
				return 0, 0, 0, 0, 0, err
			}
		} else {
			if err := json.Unmarshal(data, &hostSet); err != nil {
				return 0, 0, 0, 0, 0, err
			}
		}
		
		provider := strings.TrimSuffix(filepath.Base(file.path), ".cache")
		readTime := time.Since(readStart)
		totalReadTime += readTime
		
		// Merge hosts by name
		for i := 0; i < hostSet.Len(); i++ {
			host := hostSet.Get(i)
			
			if existing, ok := mergedHosts[host.Name]; ok {
				// Merge attributes (last provider wins for conflicts)
				for k, v := range host.Attributes {
					existing.Attributes[k] = v
				}
				existing.Providers = append(existing.Providers, provider)
				if existing.Address == "" {
					existing.Address = host.Address
				}
			} else {
				// Create new merged host
				attrs := make(map[string]interface{})
				for k, v := range host.Attributes {
					attrs[k] = v
				}
				mergedHosts[host.Name] = &MergedHost{
					Name:       host.Name,
					Address:    host.Address,
					Attributes: attrs,
					Providers:  []string{provider},
				}
			}
		}
		
		fmt.Printf("- %s: %d hosts (read: %v)\n", provider, hostSet.Len(), readTime.Round(time.Millisecond))
	}
	
	// Second pass: write merged hosts to database
	writeStart := time.Now()
	totalHosts := 0
	
	for _, mergedHost := range mergedHosts {
		// Add provider tracking
		mergedHost.Attributes["herd_provider"] = mergedHost.Providers
		
		attrJSON, _ := json.Marshal(mergedHost.Attributes)
		
		// Extract common attributes for denormalized columns
		appRole := getStringAttr(mergedHost.Attributes, "app-role")
		region := getStringAttr(mergedHost.Attributes, "region")
		site := getStringAttr(mergedHost.Attributes, "site")
		app := getStringAttr(mergedHost.Attributes, "app")
		role := getStringAttr(mergedHost.Attributes, "role")
		stamp := getStringAttr(mergedHost.Attributes, "stamp")
		
		if _, err := tx.Stmt(hostStmt).Exec(mergedHost.Name, mergedHost.Address, "merged", string(attrJSON), appRole, region, site, app, role, stamp); err != nil {
			return 0, 0, 0, 0, 0, fmt.Errorf("failed to insert host %s: %w", mergedHost.Name, err)
		}
		
		totalHosts++
	}
	
	totalWriteTime = time.Since(writeStart)
	
	if err := tx.Commit(); err != nil {
		return 0, 0, 0, 0, 0, err
	}
	
	dataImportTime := time.Since(dataImportStart)
	
	// Create indexes
	indexTime, err := createIndexes(db, jsonIndex, ftsIndex)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("failed to create indexes: %w", err)
	}
	
	fmt.Printf("Merged %d unique hosts from %d providers\n", totalHosts, len(files))
	
	return totalHosts, dataImportTime, totalReadTime, totalWriteTime, indexTime, nil
}

type MergedHost struct {
	Name       string
	Address    string
	Attributes map[string]interface{}
	Providers  []string
}

func querySQLiteDenormalized(dbPath string, filter string, dumpSQL bool) (int, error) {
	count, _, err := queryDenormalized(dbPath, filter, dumpSQL)
	return count, err
}

func queryMultiProvider(dbPath string, filter string, dumpSQL bool) (int, time.Duration, error) {
	if filter == "" {
		return queryCountAll(dbPath, filter, dumpSQL)
	}
	
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	
	// Parse multiple filters
	filters := strings.Split(filter, ",")
	
	start := time.Now()
	
	// Build UNION query with distinct match types
	var subqueries []string
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
	
	for i, f := range filters {
		parts := strings.SplitN(strings.TrimSpace(f), "=", 2)
		if len(parts) != 2 {
			continue
		}
		
		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		
		if column, ok := attrToColumn[key]; ok {
			// Use denormalized column
			subqueries = append(subqueries, fmt.Sprintf(
				"SELECT DISTINCT name, %d as match_type FROM hosts WHERE %s = ?", 
				i+1, column))
			args = append(args, value)
		} else {
			// Use JSON extraction with proper type handling
			if value == "true" || value == "false" {
				// Handle boolean values
				subqueries = append(subqueries, fmt.Sprintf(
					"SELECT DISTINCT name, %d as match_type FROM hosts WHERE json_extract(attributes_json, '$.' || ?) = %s", 
					i+1, value))
				args = append(args, key)
			} else {
				// Handle string values
				subqueries = append(subqueries, fmt.Sprintf(
					"SELECT DISTINCT name, %d as match_type FROM hosts WHERE json_extract(attributes_json, '$.' || ?) = ?", 
					i+1))
				args = append(args, key, value)
			}
		}
	}
	
	if len(subqueries) == 0 {
		return queryCountAll(dbPath, filter, dumpSQL)
	}
	
	// Build the full query
	query := fmt.Sprintf(`
		SELECT COUNT(*) FROM (
			SELECT name FROM (
				%s
			)
			GROUP BY name
			HAVING COUNT(DISTINCT match_type) = %d
		)
	`, strings.Join(subqueries, " UNION ALL "), len(subqueries))
	
	if dumpSQL {
		fmt.Printf("  SQL: %s\n", strings.TrimSpace(query))
		fmt.Printf("  Args: %v\n", args)
	}
	
	var count int
	err = db.QueryRow(query, args...).Scan(&count)
	
	return count, time.Since(start), err
}

func queryMerged(dbPath string, filter string, dumpSQL bool) (int, time.Duration, error) {
	if filter == "" {
		return queryCountAll(dbPath, filter, dumpSQL)
	}
	
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	
	// Parse multiple filters
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
			// Use denormalized column
			conditions = append(conditions, column + " = ?")
			args = append(args, value)
		} else {
			// Use JSON extraction with proper type handling
			if value == "true" || value == "false" {
				// Handle boolean values
				conditions = append(conditions, fmt.Sprintf("json_extract(attributes_json, '$.%s') = %s", key, value))
			} else {
				// Handle string values  
				conditions = append(conditions, fmt.Sprintf("json_extract(attributes_json, '$.%s') = ?", key))
				args = append(args, value)
			}
		}
	}
	
	if len(conditions) == 0 {
		return queryCountAll(dbPath, filter, dumpSQL)
	}
	
	query := "SELECT COUNT(*) FROM hosts WHERE " + strings.Join(conditions, " AND ")
	
	if dumpSQL {
		fmt.Printf("  SQL: %s\n", query)
		fmt.Printf("  Args: %v\n", args)
	}
	
	var count int
	err = db.QueryRow(query, args...).Scan(&count)
	
	return count, time.Since(start), err
}

// Check if the database has the correct indexes for the requested test mode
func hasCorrectIndexes(dbPath string, ftsIndex bool, jsonIndex bool) bool {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return false
	}
	defer db.Close()
	
	// Check if hosts table exists
	var tableExists int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='hosts'").Scan(&tableExists)
	if err != nil || tableExists == 0 {
		return false
	}
	
	// Check for required denormalized columns
	var columnCount int
	err = db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('hosts') WHERE name IN ('app_role', 'region', 'site')").Scan(&columnCount)
	if err != nil || columnCount < 3 {
		return false // Missing denormalized columns
	}
	
	// Check for denormalized indexes (always needed)
	var denormIndexes int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name LIKE 'idx_app_role%'").Scan(&denormIndexes)
	if err != nil || denormIndexes == 0 {
		return false // Missing denormalized indexes
	}
	
	// Check for JSON indexes if requested
	if jsonIndex {
		var jsonIndexes int
		err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name LIKE 'idx_json_%'").Scan(&jsonIndexes)
		if err != nil || jsonIndexes == 0 {
			return false // Missing JSON indexes
		}
	}
	
	// Check for FTS index if requested
	if ftsIndex {
		var ftsTable int
		err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='hosts_fts'").Scan(&ftsTable)
		if err != nil || ftsTable == 0 {
			return false // Missing FTS table
		}
	}
	
	return true
}