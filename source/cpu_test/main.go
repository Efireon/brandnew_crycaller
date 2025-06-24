package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const VERSION = "1.0.1"

type CPUInfo struct {
	Socket        int      `json:"socket"`       // Physical socket number
	ProcessorID   int      `json:"processor_id"` // Logical processor ID
	ModelName     string   `json:"model_name"`
	Vendor        string   `json:"vendor"`
	Family        int      `json:"family"`
	Model         int      `json:"model"`
	Stepping      int      `json:"stepping"`
	PhysicalCores int      `json:"physical_cores"`
	LogicalCores  int      `json:"logical_cores"`
	BaseFreqMHz   int      `json:"base_freq_mhz"`
	MaxFreqMHz    int      `json:"max_freq_mhz"`
	CacheL1I      int      `json:"cache_l1i_kb"` // L1 instruction cache
	CacheL1D      int      `json:"cache_l1d_kb"` // L1 data cache
	CacheL2       int      `json:"cache_l2_kb"`  // L2 cache
	CacheL3       int      `json:"cache_l3_kb"`  // L3 cache
	Flags         []string `json:"flags"`
	Temperature   float64  `json:"temperature_c"` // if available
	CurrentFreq   int      `json:"current_freq_mhz"`
}

type CPURequirement struct {
	Name             string   `json:"name"`
	MinSockets       int      `json:"min_sockets"`
	MaxSockets       int      `json:"max_sockets"`
	MinPhysicalCores int      `json:"min_physical_cores"`
	MinLogicalCores  int      `json:"min_logical_cores"`
	MinBaseFreqMHz   int      `json:"min_base_freq_mhz"`
	MinCacheL3KB     int      `json:"min_cache_l3_kb"`
	RequiredVendor   string   `json:"required_vendor"`
	RequiredFlags    []string `json:"required_flags"`
	MaxTempC         float64  `json:"max_temp_c"`
}

type CPUVisual struct {
	Symbol      string `json:"symbol"`
	ShortName   string `json:"short_name"`
	Description string `json:"description"`
	Color       string `json:"color"`
}

type VisualizationConfig struct {
	SocketVisuals map[string]CPUVisual `json:"socket_visuals"` // vendor -> visual
	TotalSockets  int                  `json:"total_sockets"`
	SlotWidth     int                  `json:"slot_width"`
}

type Config struct {
	CPURequirements []CPURequirement    `json:"cpu_requirements"`
	Visualization   VisualizationConfig `json:"visualization"`
	CheckTemp       bool                `json:"check_temperature"`
	CheckFreq       bool                `json:"check_frequency"`
	CheckCache      bool                `json:"check_cache"`
}

type CPUCheckResult struct {
	Status    string // "ok", "warning", "error", "missing"
	Issues    []string
	CoresOK   bool
	FreqOK    bool
	CacheOK   bool
	TempOK    bool
	FlagsOK   bool
	CoresWarn bool
	FreqWarn  bool
	CacheWarn bool
	TempWarn  bool
	FlagsWarn bool
}

// ANSI color codes
const (
	ColorReset  = "\033[0m"
	ColorGreen  = "\033[92m"
	ColorBlue   = "\033[34m"
	ColorWhite  = "\033[37m"
	ColorYellow = "\033[33m"
	ColorRed    = "\033[31m"
)

var debugMode bool

func printColored(color, message string) {
	fmt.Printf("%s%s%s\n", color, message, ColorReset)
}

func printSuccess(message string) {
	printColored(ColorGreen, message)
}

func printInfo(message string) {
	printColored(ColorBlue, message)
}

func printDebug(message string) {
	if debugMode {
		printColored(ColorWhite, message)
	}
}

func printWarning(message string) {
	printColored(ColorYellow, message)
}

func printError(message string) {
	printColored(ColorRed, message)
}

func showHelp() {
	fmt.Printf("CPU Checker %s\n", VERSION)
	fmt.Println("Parameters:")
	fmt.Println("  -V          Show program version")
	fmt.Println("  -c <path>   Path to configuration file")
	fmt.Println("  -s          Create default configuration file")
	fmt.Println("  -l          List detected CPUs without configuration check")
	fmt.Println("  -vis        Show visual CPU sockets layout")
	fmt.Println("  -d          Show detailed debug information")
	fmt.Println("  -h          Show this help")
}

func getCPUInfo() ([]CPUInfo, error) {
	var cpus []CPUInfo

	// Read /proc/cpuinfo
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return nil, fmt.Errorf("failed to open /proc/cpuinfo: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var currentCPU CPUInfo
	var processorID int = -1

	for scanner.Scan() {
		line := scanner.Text()

		if strings.TrimSpace(line) == "" {
			// End of processor block
			if processorID >= 0 {
				currentCPU.ProcessorID = processorID
				cpus = append(cpus, currentCPU)
				currentCPU = CPUInfo{}
				processorID = -1
			}
			continue
		}

		parts := strings.Split(line, ":")
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch key {
		case "processor":
			if pid, err := strconv.Atoi(value); err == nil {
				processorID = pid
			}
		case "physical id":
			if socket, err := strconv.Atoi(value); err == nil {
				currentCPU.Socket = socket
			}
		case "model name":
			currentCPU.ModelName = value
		case "vendor_id":
			currentCPU.Vendor = value
		case "cpu family":
			if family, err := strconv.Atoi(value); err == nil {
				currentCPU.Family = family
			}
		case "model":
			if model, err := strconv.Atoi(value); err == nil {
				currentCPU.Model = model
			}
		case "stepping":
			if stepping, err := strconv.Atoi(value); err == nil {
				currentCPU.Stepping = stepping
			}
		case "cpu cores":
			if cores, err := strconv.Atoi(value); err == nil {
				currentCPU.PhysicalCores = cores
			}
		case "siblings":
			if siblings, err := strconv.Atoi(value); err == nil {
				currentCPU.LogicalCores = siblings
			}
		case "cpu MHz":
			if freq, err := strconv.ParseFloat(value, 64); err == nil {
				currentCPU.CurrentFreq = int(freq)
			}
		case "cache size":
			// Parse cache size like "8192 KB"
			if cacheStr := strings.Fields(value); len(cacheStr) >= 2 {
				if cache, err := strconv.Atoi(cacheStr[0]); err == nil {
					// This is usually L3 cache
					currentCPU.CacheL3 = cache
				}
			}
		case "flags":
			currentCPU.Flags = strings.Fields(value)
		}
	}

	// Add last processor if exists
	if processorID >= 0 {
		currentCPU.ProcessorID = processorID
		cpus = append(cpus, currentCPU)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading /proc/cpuinfo: %v", err)
	}

	// Get additional frequency information from /proc/cpuinfo or /sys
	for i := range cpus {
		cpus[i] = enrichCPUInfo(cpus[i])
	}

	// Deduplicate to get unique physical CPUs
	uniqueCPUs := deduplicateCPUs(cpus)

	if debugMode {
		printDebug(fmt.Sprintf("Found %d logical processors, %d unique physical CPUs", len(cpus), len(uniqueCPUs)))
	}

	return uniqueCPUs, nil
}

func enrichCPUInfo(cpu CPUInfo) CPUInfo {
	// Try to get base frequency from scaling_driver
	if cpu.BaseFreqMHz == 0 {
		// Try to read from /sys/devices/system/cpu/cpu0/cpufreq/
		if freq := readSysFreq("cpuinfo_min_freq"); freq > 0 {
			cpu.BaseFreqMHz = freq / 1000 // Convert from kHz to MHz
		}
	}

	if cpu.MaxFreqMHz == 0 {
		if freq := readSysFreq("cpuinfo_max_freq"); freq > 0 {
			cpu.MaxFreqMHz = freq / 1000 // Convert from kHz to MHz
		}
	}

	// Try to get temperature
	cpu.Temperature = readCPUTemperature(cpu.Socket)

	// Try to get L1/L2 cache sizes
	cpu.CacheL1I, cpu.CacheL1D, cpu.CacheL2 = readCacheSizes()

	return cpu
}

func readSysFreq(filename string) int {
	data, err := os.ReadFile(fmt.Sprintf("/sys/devices/system/cpu/cpu0/cpufreq/%s", filename))
	if err != nil {
		return 0
	}

	if freq, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
		return freq
	}
	return 0
}

func readCPUTemperature(socket int) float64 {
	// Try different thermal zone paths
	thermalPaths := []string{
		fmt.Sprintf("/sys/class/thermal/thermal_zone%d/temp", socket),
		"/sys/class/thermal/thermal_zone0/temp",
		"/sys/class/hwmon/hwmon0/temp1_input",
	}

	for _, path := range thermalPaths {
		if data, err := os.ReadFile(path); err == nil {
			if temp, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				return float64(temp) / 1000.0 // Convert from millidegrees
			}
		}
	}
	return 0
}

func readCacheSizes() (l1i, l1d, l2 int) {
	// Try to read cache sizes from /sys
	cachePaths := map[string]*int{
		"/sys/devices/system/cpu/cpu0/cache/index0/size": &l1d, // L1 data
		"/sys/devices/system/cpu/cpu0/cache/index1/size": &l1i, // L1 instruction
		"/sys/devices/system/cpu/cpu0/cache/index2/size": &l2,  // L2
	}

	for path, target := range cachePaths {
		if data, err := os.ReadFile(path); err == nil {
			sizeStr := strings.TrimSpace(string(data))
			if size := parseCacheSize(sizeStr); size > 0 {
				*target = size
			}
		}
	}
	return
}

func parseCacheSize(sizeStr string) int {
	// Parse sizes like "32K", "256K", "8M"
	re := regexp.MustCompile(`^(\d+)([KMG]?)$`)
	matches := re.FindStringSubmatch(sizeStr)

	if len(matches) < 2 {
		return 0
	}

	size, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0
	}

	unit := "K"
	if len(matches) > 2 && matches[2] != "" {
		unit = matches[2]
	}

	switch unit {
	case "K":
		return size
	case "M":
		return size * 1024
	case "G":
		return size * 1024 * 1024
	default:
		return size
	}
}

func deduplicateCPUs(cpus []CPUInfo) []CPUInfo {
	socketMap := make(map[int]CPUInfo)

	for _, cpu := range cpus {
		if existing, exists := socketMap[cpu.Socket]; !exists {
			socketMap[cpu.Socket] = cpu
		} else {
			// Keep the one with more complete information
			if cpu.ModelName != "" && existing.ModelName == "" {
				socketMap[cpu.Socket] = cpu
			}
		}
	}

	var result []CPUInfo
	for _, cpu := range socketMap {
		result = append(result, cpu)
	}

	return result
}

func createDefaultConfig(configPath string) error {
	printInfo("Scanning system for CPU information to create configuration...")

	cpus, err := getCPUInfo()
	if err != nil {
		return fmt.Errorf("could not scan CPUs: %v", err)
	}

	if len(cpus) == 0 {
		return fmt.Errorf("no CPU sockets found - cannot create configuration")
	}

	printInfo(fmt.Sprintf("Found %d CPU socket(s), creating configuration:", len(cpus)))

	for i, cpu := range cpus {
		printInfo(fmt.Sprintf("  Socket %d: %s", cpu.Socket, cpu.ModelName))
		printInfo(fmt.Sprintf("    Cores: %d physical, %d logical", cpu.PhysicalCores, cpu.LogicalCores))
		if cpu.BaseFreqMHz > 0 || cpu.MaxFreqMHz > 0 {
			printInfo(fmt.Sprintf("    Frequency: %d MHz base, %d MHz max", cpu.BaseFreqMHz, cpu.MaxFreqMHz))
		}
		if cpu.CacheL3 > 0 {
			printInfo(fmt.Sprintf("    L3 Cache: %d KB (cache checking disabled in config)", cpu.CacheL3))
		}
		if cpu.Temperature > 0 {
			printInfo(fmt.Sprintf("    Temperature: %.1f°C", cpu.Temperature))
		}
		_ = i
	}

	// Group CPUs by vendor
	vendorGroups := make(map[string][]CPUInfo)
	for _, cpu := range cpus {
		vendorGroups[cpu.Vendor] = append(vendorGroups[cpu.Vendor], cpu)
	}

	var requirements []CPURequirement
	socketVisuals := make(map[string]CPUVisual)

	for vendor, vendorCPUs := range vendorGroups {
		printInfo(fmt.Sprintf("  Processing %d %s socket(s):", len(vendorCPUs), vendor))

		// Find minimum values across all sockets of this vendor
		minPhysicalCores := vendorCPUs[0].PhysicalCores
		minLogicalCores := vendorCPUs[0].LogicalCores
		minBaseFreq := vendorCPUs[0].BaseFreqMHz

		var commonFlags []string
		flagCounts := make(map[string]int)

		for _, cpu := range vendorCPUs {
			if cpu.PhysicalCores < minPhysicalCores && cpu.PhysicalCores > 0 {
				minPhysicalCores = cpu.PhysicalCores
			}
			if cpu.LogicalCores < minLogicalCores && cpu.LogicalCores > 0 {
				minLogicalCores = cpu.LogicalCores
			}
			if cpu.BaseFreqMHz < minBaseFreq && cpu.BaseFreqMHz > 0 {
				minBaseFreq = cpu.BaseFreqMHz
			}

			// Count flags
			for _, flag := range cpu.Flags {
				flagCounts[flag]++
			}
		}

		// Select flags that appear in all CPUs of this vendor
		for flag, count := range flagCounts {
			if count == len(vendorCPUs) {
				// Only include important flags
				if isImportantFlag(flag) {
					commonFlags = append(commonFlags, flag)
				}
			}
		}

		req := CPURequirement{
			Name:             fmt.Sprintf("%s CPUs (sockets: %d)", capitalizeVendor(vendor), len(vendorCPUs)),
			MinSockets:       len(vendorCPUs),
			MaxSockets:       len(vendorCPUs),
			MinPhysicalCores: minPhysicalCores,
			MinLogicalCores:  minLogicalCores,
			MinBaseFreqMHz:   minBaseFreq,
			RequiredVendor:   vendor,
			RequiredFlags:    commonFlags,
			MaxTempC:         80.0, // Default temperature limit
		}

		requirements = append(requirements, req)

		// Create visual for this vendor
		visual := generateCPUVisual(vendor)
		socketVisuals[vendor] = visual
		printInfo(fmt.Sprintf("    Visual: %s (%s)", visual.Symbol, visual.ShortName))
	}

	config := Config{
		CPURequirements: requirements,
		Visualization: VisualizationConfig{
			SocketVisuals: socketVisuals,
			TotalSockets:  len(cpus), // Found sockets
			SlotWidth:     12,
		},
		CheckTemp:  true,
		CheckFreq:  true,
		CheckCache: false, // Cache checking disabled by default
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	err = os.MkdirAll(filepath.Dir(configPath), 0755)
	if err != nil {
		return err
	}

	err = os.WriteFile(configPath, data, 0644)
	if err != nil {
		return err
	}

	printSuccess("Configuration created successfully based on detected hardware")
	printInfo(fmt.Sprintf("Total CPU sockets: %d", len(cpus)))
	printInfo("Cache checking is disabled by default (can be enabled in config)")
	printInfo("You can edit the configuration file to adjust requirements as needed")

	return nil
}

func isImportantFlag(flag string) bool {
	importantFlags := []string{
		"sse", "sse2", "sse3", "ssse3", "sse4_1", "sse4_2",
		"avx", "avx2", "avx512f",
		"aes", "rdrand", "rdseed",
		"hypervisor", "vmx", "svm",
		"x2apic", "tsc", "constant_tsc",
	}

	for _, important := range importantFlags {
		if flag == important {
			return true
		}
	}
	return false
}

func generateCPUVisual(vendor string) CPUVisual {
	visual := CPUVisual{
		Description: fmt.Sprintf("%s CPU", capitalizeVendor(vendor)),
		Color:       "green",
	}

	switch strings.ToLower(vendor) {
	case "genuineintel":
		visual.Symbol = "████"
		visual.ShortName = "INTEL"
	case "authenticamd":
		visual.Symbol = "▓▓▓▓"
		visual.ShortName = "AMD"
	default:
		visual.Symbol = "░░░░"
		visual.ShortName = strings.ToUpper(vendor[:min(len(vendor), 5)])
	}

	return visual
}

func capitalizeVendor(vendor string) string {
	switch strings.ToLower(vendor) {
	case "genuineintel":
		return "Intel"
	case "authenticamd":
		return "AMD"
	default:
		return vendor
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func checkCPUAgainstRequirements(cpu CPUInfo, config *Config) CPUCheckResult {
	result := CPUCheckResult{
		Status:  "ok",
		CoresOK: true,
		FreqOK:  true,
		CacheOK: true,
		TempOK:  true,
		FlagsOK: true,
	}

	var matchingReqs []CPURequirement
	for _, req := range config.CPURequirements {
		if req.RequiredVendor != "" && req.RequiredVendor != cpu.Vendor {
			continue
		}
		matchingReqs = append(matchingReqs, req)
	}

	if len(matchingReqs) == 0 {
		return result
	}

	hasErrors := false
	hasWarnings := false

	for _, req := range matchingReqs {
		// Check cores
		if req.MinPhysicalCores > 0 && cpu.PhysicalCores < req.MinPhysicalCores {
			result.Issues = append(result.Issues,
				fmt.Sprintf("Physical cores: %d (required %d)", cpu.PhysicalCores, req.MinPhysicalCores))
			result.CoresOK = false
			hasErrors = true
		}

		if req.MinLogicalCores > 0 && cpu.LogicalCores < req.MinLogicalCores {
			result.Issues = append(result.Issues,
				fmt.Sprintf("Logical cores: %d (required %d)", cpu.LogicalCores, req.MinLogicalCores))
			result.CoresOK = false
			hasErrors = true
		}

		// Check frequency
		if req.MinBaseFreqMHz > 0 {
			if cpu.BaseFreqMHz == 0 {
				result.Issues = append(result.Issues, "Could not determine base frequency")
				result.FreqWarn = true
				hasWarnings = true
			} else if cpu.BaseFreqMHz < req.MinBaseFreqMHz {
				result.Issues = append(result.Issues,
					fmt.Sprintf("Base frequency: %d MHz (required %d MHz)", cpu.BaseFreqMHz, req.MinBaseFreqMHz))
				result.FreqOK = false
				hasErrors = true
			}
		}

		// Check cache (only if enabled in config)
		if config.CheckCache && req.MinCacheL3KB > 0 {
			if cpu.CacheL3 == 0 {
				result.Issues = append(result.Issues, "Could not determine L3 cache size")
				result.CacheWarn = true
				hasWarnings = true
			} else if cpu.CacheL3 < req.MinCacheL3KB {
				result.Issues = append(result.Issues,
					fmt.Sprintf("L3 cache: %d KB (required %d KB)", cpu.CacheL3, req.MinCacheL3KB))
				result.CacheOK = false
				hasErrors = true
			}
		}

		// Check temperature
		if req.MaxTempC > 0 && cpu.Temperature > 0 {
			if cpu.Temperature > req.MaxTempC {
				result.Issues = append(result.Issues,
					fmt.Sprintf("Temperature: %.1f°C (max %.1f°C)", cpu.Temperature, req.MaxTempC))
				result.TempOK = false
				hasErrors = true
			}
		}

		// Check required flags
		for _, reqFlag := range req.RequiredFlags {
			found := false
			for _, cpuFlag := range cpu.Flags {
				if cpuFlag == reqFlag {
					found = true
					break
				}
			}
			if !found {
				result.Issues = append(result.Issues, fmt.Sprintf("Missing CPU flag: %s", reqFlag))
				result.FlagsOK = false
				hasErrors = true
			}
		}
	}

	if hasErrors {
		result.Status = "error"
	} else if hasWarnings {
		result.Status = "warning"
	}

	return result
}

func centerText(text string, width int) string {
	if len(text) == 0 {
		return strings.Repeat(" ", width)
	}

	runes := []rune(text)
	runeLen := len(runes)

	if runeLen > width {
		if width > 0 {
			return string(runes[:width])
		}
		return ""
	}

	if runeLen == width {
		return text
	}

	padding := width - runeLen
	leftPad := padding / 2
	rightPad := padding - leftPad

	return strings.Repeat(" ", leftPad) + text + strings.Repeat(" ", rightPad)
}

func formatFreq(freqMHz int) string {
	if freqMHz == 0 {
		return "?"
	}
	if freqMHz < 1000 {
		return fmt.Sprintf("%dMHz", freqMHz)
	} else {
		return fmt.Sprintf("%.1fGHz", float64(freqMHz)/1000.0)
	}
}

func formatCache(cacheKB int) string {
	if cacheKB == 0 {
		return "?"
	}
	if cacheKB < 1024 {
		return fmt.Sprintf("%dKB", cacheKB)
	} else {
		return fmt.Sprintf("%dMB", cacheKB/1024)
	}
}

func formatTemp(temp float64) string {
	if temp == 0 {
		return "?"
	}
	return fmt.Sprintf("%.1f°C", temp)
}

func visualizeSockets(cpus []CPUInfo, config *Config) error {
	printInfo("CPU Sockets Layout:")
	fmt.Println()

	maxSockets := config.Visualization.TotalSockets
	if maxSockets == 0 {
		maxSockets = len(cpus) + 2
	}

	// Create socket data array
	socketData := make([]CPUInfo, maxSockets+1) // +1 because sockets start from 1
	socketResults := make([]CPUCheckResult, maxSockets+1)

	// Fill sockets
	expectedSockets := make(map[int]bool)
	for _, cpu := range cpus {
		socket := cpu.Socket + 1 // Convert to 1-based indexing for display
		if socket > 0 && socket <= maxSockets {
			socketData[socket] = cpu
			expectedSockets[socket] = true
			socketResults[socket] = checkCPUAgainstRequirements(cpu, config)
		}
	}

	// Check for missing CPUs and status summary
	hasErrors := false
	hasWarnings := false
	missingCPUs := []string{}

	for i := 1; i <= len(cpus); i++ {
		if !expectedSockets[i] {
			socketResults[i] = CPUCheckResult{Status: "missing"}
			missingCPUs = append(missingCPUs, fmt.Sprintf("Socket %d", i))
			hasErrors = true

			socketData[i] = CPUInfo{
				ModelName: "MISSING",
				Socket:    i - 1,
			}
		}
	}

	// Count status types
	for i := 1; i <= maxSockets; i++ {
		status := socketResults[i].Status
		if status == "error" || status == "missing" {
			hasErrors = true
		} else if status == "warning" {
			hasWarnings = true
		}
	}

	// Print legend
	printInfo("Legend:")
	fmt.Printf("  %s%s%s CPU Working Correctly  ", ColorGreen, "████", ColorReset)
	fmt.Printf("  %s%s%s CPU with Issues  ", ColorYellow, "████", ColorReset)
	fmt.Printf("  %s%s%s Missing CPU  ", ColorRed, "░░░░", ColorReset)
	fmt.Printf("  %s%s%s Empty Socket\n", ColorWhite, "░░░░", ColorReset)
	fmt.Println()

	// Report missing CPUs
	if len(missingCPUs) > 0 {
		printError("Missing CPUs:")
		for _, cpu := range missingCPUs {
			printError(fmt.Sprintf("  - %s", cpu))
		}
		fmt.Println()
	}

	// Report detailed issues
	for i := 1; i <= maxSockets; i++ {
		result := socketResults[i]
		if len(result.Issues) > 0 {
			if result.Status == "error" {
				printError(fmt.Sprintf("Socket %d issues:", i))
			} else if result.Status == "warning" {
				printWarning(fmt.Sprintf("Socket %d warnings:", i))
			}

			for _, issue := range result.Issues {
				if result.Status == "error" {
					printError(fmt.Sprintf("  - %s", issue))
				} else if result.Status == "warning" {
					printWarning(fmt.Sprintf("  - %s", issue))
				}
			}
		}
	}

	if hasErrors || hasWarnings {
		fmt.Println()
	}

	// Build visualization
	width := config.Visualization.SlotWidth

	// Top border
	fmt.Print("┌")
	for i := 1; i <= maxSockets; i++ {
		fmt.Print(strings.Repeat("─", width))
		if i < maxSockets {
			fmt.Print("┬")
		}
	}
	fmt.Println("┐")

	// Symbols row
	fmt.Print("│")
	for i := 1; i <= maxSockets; i++ {
		visual := getCPUVisual(socketData[i], &config.Visualization)
		result := socketResults[i]

		symbolText := centerText(visual.Symbol, width)

		switch result.Status {
		case "ok":
			fmt.Print(ColorGreen + symbolText + ColorReset)
		case "warning", "error":
			fmt.Print(ColorYellow + symbolText + ColorReset)
		case "missing":
			fmt.Print(ColorRed + centerText("░░░░", width) + ColorReset)
		default:
			fmt.Print(symbolText)
		}
		fmt.Print("│")
	}
	fmt.Println()

	// Vendor row
	fmt.Print("│")
	for i := 1; i <= maxSockets; i++ {
		result := socketResults[i]

		if socketData[i].ModelName != "" {
			visual := getCPUVisual(socketData[i], &config.Visualization)
			nameText := centerText(visual.ShortName, width)

			switch result.Status {
			case "ok":
				fmt.Print(ColorGreen + nameText + ColorReset)
			case "warning", "error":
				fmt.Print(ColorYellow + nameText + ColorReset)
			case "missing":
				fmt.Print(ColorRed + centerText("MISS", width) + ColorReset)
			default:
				fmt.Print(nameText)
			}
		} else {
			fmt.Print(strings.Repeat(" ", width))
		}
		fmt.Print("│")
	}
	fmt.Println()

	// Cores row
	fmt.Print("│")
	for i := 1; i <= maxSockets; i++ {
		result := socketResults[i]

		if socketData[i].ModelName != "" {
			var coreInfo string
			if result.Status == "missing" {
				coreInfo = "?"
			} else {
				coreInfo = fmt.Sprintf("%dC/%dT", socketData[i].PhysicalCores, socketData[i].LogicalCores)
			}

			coreText := centerText(coreInfo, width)

			if result.Status == "missing" {
				fmt.Print(ColorRed + coreText + ColorReset)
			} else if !result.CoresOK {
				fmt.Print(ColorRed + coreText + ColorReset)
			} else if result.Status == "warning" || result.Status == "error" {
				fmt.Print(ColorYellow + coreText + ColorReset)
			} else if result.Status == "ok" {
				fmt.Print(ColorGreen + coreText + ColorReset)
			} else {
				fmt.Print(coreText)
			}
		} else {
			fmt.Print(strings.Repeat(" ", width))
		}
		fmt.Print("│")
	}
	fmt.Println()

	// Frequency row
	fmt.Print("│")
	for i := 1; i <= maxSockets; i++ {
		result := socketResults[i]

		if socketData[i].ModelName != "" {
			var freqInfo string
			if result.Status == "missing" {
				freqInfo = "?"
			} else {
				freqInfo = formatFreq(socketData[i].BaseFreqMHz)
			}

			freqText := centerText(freqInfo, width)

			if result.Status == "missing" {
				fmt.Print(ColorRed + freqText + ColorReset)
			} else if !result.FreqOK {
				fmt.Print(ColorRed + freqText + ColorReset)
			} else if result.Status == "warning" || result.Status == "error" {
				fmt.Print(ColorYellow + freqText + ColorReset)
			} else if result.FreqWarn {
				fmt.Print(ColorYellow + freqText + ColorReset)
			} else if result.Status == "ok" {
				fmt.Print(ColorGreen + freqText + ColorReset)
			} else {
				fmt.Print(freqText)
			}
		} else {
			fmt.Print(strings.Repeat(" ", width))
		}
		fmt.Print("│")
	}
	fmt.Println()

	// Temperature row
	fmt.Print("│")
	for i := 1; i <= maxSockets; i++ {
		result := socketResults[i]

		if socketData[i].ModelName != "" {
			var tempInfo string
			if result.Status == "missing" {
				tempInfo = "?"
			} else {
				tempInfo = formatTemp(socketData[i].Temperature)
			}

			tempText := centerText(tempInfo, width)

			if result.Status == "missing" {
				fmt.Print(ColorRed + tempText + ColorReset)
			} else if !result.TempOK {
				fmt.Print(ColorRed + tempText + ColorReset)
			} else if result.Status == "warning" || result.Status == "error" {
				fmt.Print(ColorYellow + tempText + ColorReset)
			} else if result.TempWarn {
				fmt.Print(ColorYellow + tempText + ColorReset)
			} else if result.Status == "ok" {
				fmt.Print(ColorGreen + tempText + ColorReset)
			} else {
				fmt.Print(tempText)
			}
		} else {
			fmt.Print(strings.Repeat(" ", width))
		}
		fmt.Print("│")
	}
	fmt.Println()

	// Cache row (only if cache checking is enabled)
	if config.CheckCache {
		fmt.Print("│")
		for i := 1; i <= maxSockets; i++ {
			result := socketResults[i]

			if socketData[i].ModelName != "" {
				var cacheInfo string
				if result.Status == "missing" {
					cacheInfo = "?"
				} else {
					cacheInfo = formatCache(socketData[i].CacheL3)
				}

				cacheText := centerText(cacheInfo, width)

				if result.Status == "missing" {
					fmt.Print(ColorRed + cacheText + ColorReset)
				} else if !result.CacheOK {
					fmt.Print(ColorRed + cacheText + ColorReset)
				} else if result.Status == "warning" || result.Status == "error" {
					fmt.Print(ColorYellow + cacheText + ColorReset)
				} else if result.CacheWarn {
					fmt.Print(ColorYellow + cacheText + ColorReset)
				} else if result.Status == "ok" {
					fmt.Print(ColorGreen + cacheText + ColorReset)
				} else {
					fmt.Print(cacheText)
				}
			} else {
				fmt.Print(strings.Repeat(" ", width))
			}
			fmt.Print("│")
		}
		fmt.Println()
	}

	// Bottom border
	fmt.Print("└")
	for i := 1; i <= maxSockets; i++ {
		fmt.Print(strings.Repeat("─", width))
		if i < maxSockets {
			fmt.Print("┴")
		}
	}
	fmt.Println("┘")

	// Socket numbers
	fmt.Print(" ")
	for i := 1; i <= maxSockets; i++ {
		fmt.Print(centerText(fmt.Sprintf("Socket %d", i), width+1))
	}
	fmt.Println()

	fmt.Println()

	// Final status
	if hasErrors {
		printError("CPU configuration validation FAILED!")
		return fmt.Errorf("CPU configuration validation failed")
	} else if hasWarnings {
		printWarning("CPU configuration validation completed with warnings")
		return nil
	} else {
		printSuccess("All CPUs present and meet requirements!")
		return nil
	}
}

func getCPUVisual(cpu CPUInfo, config *VisualizationConfig) CPUVisual {
	if cpu.ModelName == "" {
		return CPUVisual{
			Symbol:      "░░░░",
			ShortName:   "",
			Description: "Empty Socket",
			Color:       "gray",
		}
	}

	if visual, exists := config.SocketVisuals[cpu.Vendor]; exists {
		return visual
	}

	return generateCPUVisual(cpu.Vendor)
}

func checkCPU(config *Config) error {
	printInfo("Starting CPU check...")

	cpus, err := getCPUInfo()
	if err != nil {
		return fmt.Errorf("failed to get CPU info: %v", err)
	}

	printInfo(fmt.Sprintf("Found CPU sockets: %d", len(cpus)))

	if len(cpus) == 0 {
		printError("No CPU sockets found")
		return fmt.Errorf("no CPU sockets found")
	}

	// Display found CPUs
	for i, cpu := range cpus {
		printInfo(fmt.Sprintf("Socket %d: %s", i+1, cpu.ModelName))
		printDebug(fmt.Sprintf("  Physical Socket: %d", cpu.Socket))
		printDebug(fmt.Sprintf("  Vendor: %s", cpu.Vendor))
		printDebug(fmt.Sprintf("  Cores: %d physical, %d logical", cpu.PhysicalCores, cpu.LogicalCores))
		if cpu.BaseFreqMHz > 0 {
			printDebug(fmt.Sprintf("  Base Frequency: %d MHz", cpu.BaseFreqMHz))
		}
		if cpu.CacheL3 > 0 {
			if config.CheckCache {
				printDebug(fmt.Sprintf("  L3 Cache: %d KB", cpu.CacheL3))
			} else {
				printDebug(fmt.Sprintf("  L3 Cache: %d KB (checking disabled)", cpu.CacheL3))
			}
		}
		if cpu.Temperature > 0 {
			printDebug(fmt.Sprintf("  Temperature: %.1f°C", cpu.Temperature))
		}
	}

	// Check each requirement
	allPassed := true
	for _, req := range config.CPURequirements {
		printInfo(fmt.Sprintf("Checking requirement: %s", req.Name))

		matchingCPUs := filterCPUs(cpus, req)

		printInfo(fmt.Sprintf("  Found %d socket(s) matching criteria", len(matchingCPUs)))

		if len(matchingCPUs) < req.MinSockets {
			printError(fmt.Sprintf("  Requirement FAILED: found %d socket(s), required %d", len(matchingCPUs), req.MinSockets))
			allPassed = false
			continue
		}

		if req.MaxSockets > 0 && len(matchingCPUs) > req.MaxSockets {
			printError(fmt.Sprintf("  Requirement FAILED: found %d socket(s), maximum %d", len(matchingCPUs), req.MaxSockets))
			allPassed = false
			continue
		}

		// Check each matching CPU
		reqPassed := true
		for i, cpu := range matchingCPUs {
			printInfo(fmt.Sprintf("    Socket %d: %s", i+1, cpu.ModelName))

			result := checkCPUAgainstRequirements(cpu, config)

			if len(result.Issues) > 0 {
				for _, issue := range result.Issues {
					if result.Status == "error" {
						printError(fmt.Sprintf("      %s", issue))
						reqPassed = false
					} else {
						printWarning(fmt.Sprintf("      %s", issue))
					}
				}
			} else {
				printSuccess(fmt.Sprintf("      All checks passed"))
			}
		}

		if reqPassed {
			printSuccess(fmt.Sprintf("  Requirement PASSED: %s", req.Name))
		} else {
			printError(fmt.Sprintf("  Requirement FAILED: %s", req.Name))
			allPassed = false
		}
	}

	if allPassed {
		printSuccess("All CPU requirements passed")
	} else {
		printError("Some CPU requirements failed")
		return fmt.Errorf("CPU requirements not met")
	}

	return nil
}

func filterCPUs(cpus []CPUInfo, req CPURequirement) []CPUInfo {
	var matching []CPUInfo

	for _, cpu := range cpus {
		if req.RequiredVendor != "" && req.RequiredVendor != cpu.Vendor {
			continue
		}
		matching = append(matching, cpu)
	}

	return matching
}

func main() {
	var (
		showVersion  = flag.Bool("V", false, "Show version")
		configPath   = flag.String("c", "cpu_config.json", "Path to configuration file")
		createConfig = flag.Bool("s", false, "Create default configuration file")
		showHelpFlag = flag.Bool("h", false, "Show help")
		listOnly     = flag.Bool("l", false, "List detected CPUs without configuration check")
		visualize    = flag.Bool("vis", false, "Show visual CPU sockets layout")
		debugFlag    = flag.Bool("d", false, "Show detailed debug information")
	)

	flag.Parse()

	debugMode = *debugFlag

	if *showHelpFlag {
		showHelp()
		return
	}

	if *showVersion {
		fmt.Println(VERSION)
		return
	}

	if *listOnly {
		printInfo("Scanning for CPU information...")
		cpus, err := getCPUInfo()
		if err != nil {
			printError(fmt.Sprintf("Error getting CPU information: %v", err))
			os.Exit(1)
		}

		if len(cpus) == 0 {
			printWarning("No CPU sockets found")
		} else {
			printSuccess(fmt.Sprintf("Found CPU sockets: %d", len(cpus)))
			for i, cpu := range cpus {
				fmt.Printf("\nSocket %d:\n", i+1)
				fmt.Printf("  Model: %s\n", cpu.ModelName)
				fmt.Printf("  Vendor: %s\n", cpu.Vendor)
				fmt.Printf("  Physical Socket: %d\n", cpu.Socket)
				fmt.Printf("  Physical Cores: %d\n", cpu.PhysicalCores)
				fmt.Printf("  Logical Cores: %d\n", cpu.LogicalCores)
				fmt.Printf("  Base Frequency: %s\n", formatFreq(cpu.BaseFreqMHz))
				fmt.Printf("  Max Frequency: %s\n", formatFreq(cpu.MaxFreqMHz))
				fmt.Printf("  Current Frequency: %s\n", formatFreq(cpu.CurrentFreq))
				fmt.Printf("  L1I Cache: %s\n", formatCache(cpu.CacheL1I))
				fmt.Printf("  L1D Cache: %s\n", formatCache(cpu.CacheL1D))
				fmt.Printf("  L2 Cache: %s\n", formatCache(cpu.CacheL2))
				fmt.Printf("  L3 Cache: %s\n", formatCache(cpu.CacheL3))
				fmt.Printf("  Temperature: %s\n", formatTemp(cpu.Temperature))
				if len(cpu.Flags) > 0 {
					fmt.Printf("  Key Flags: ")
					flagCount := 0
					for _, flag := range cpu.Flags {
						if isImportantFlag(flag) {
							if flagCount > 0 {
								fmt.Printf(", ")
							}
							fmt.Printf("%s", flag)
							flagCount++
							if flagCount >= 10 { // Limit output
								fmt.Printf(", ...")
								break
							}
						}
					}
					fmt.Println()
				}
			}
		}
		return
	}

	if *visualize {
		printInfo("Scanning for CPU information...")
		cpus, err := getCPUInfo()
		if err != nil {
			printError(fmt.Sprintf("Error getting CPU information: %v", err))
			os.Exit(1)
		}

		config, err := loadConfig(*configPath)
		if err != nil {
			printError(fmt.Sprintf("Error loading configuration: %v", err))
			printInfo("Use -s to create a default configuration file")
			os.Exit(1)
		}

		err = visualizeSockets(cpus, config)
		if err != nil {
			os.Exit(1)
		}
		return
	}

	if *createConfig {
		printInfo(fmt.Sprintf("Creating configuration file: %s", *configPath))
		err := createDefaultConfig(*configPath)
		if err != nil {
			printError(fmt.Sprintf("Error creating configuration: %v", err))
			os.Exit(1)
		}
		printSuccess("Configuration file created successfully")
		return
	}

	// Load configuration and perform check
	config, err := loadConfig(*configPath)
	if err != nil {
		printError(fmt.Sprintf("Error loading configuration: %v", err))
		printInfo("Use -s to create a default configuration file")
		printInfo("Or use -l to simply display found CPUs")
		os.Exit(1)
	}

	printInfo(fmt.Sprintf("Configuration loaded from: %s", *configPath))

	err = checkCPU(config)
	if err != nil {
		printError(fmt.Sprintf("CPU check failed: %v", err))
		os.Exit(1)
	}
}
