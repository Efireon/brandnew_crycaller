package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const VERSION = "1.0.1"

type RAMInfo struct {
	Slot         string  `json:"slot"`          // Memory slot identifier (e.g., "DIMM_A1", "ChannelA-DIMM0")
	SizeMB       int     `json:"size_mb"`       // Size in MB
	Type         string  `json:"type"`          // DDR3, DDR4, DDR5, etc.
	Speed        int     `json:"speed_mhz"`     // Speed in MHz
	Manufacturer string  `json:"manufacturer"`  // Memory manufacturer
	PartNumber   string  `json:"part_number"`   // Part number
	SerialNumber string  `json:"serial_number"` // Serial number
	Bank         string  `json:"bank"`          // Memory bank
	IsECC        bool    `json:"is_ecc"`        // ECC support
	IsPresent    bool    `json:"is_present"`    // Whether module is installed
	Voltage      float64 `json:"voltage"`       // Operating voltage
}

type RAMRequirement struct {
	Name           string   `json:"name"`
	MinTotalSizeGB int      `json:"min_total_size_gb"`
	MaxTotalSizeGB int      `json:"max_total_size_gb"`
	MinModules     int      `json:"min_modules"`
	MaxModules     int      `json:"max_modules"`
	RequiredType   string   `json:"required_type"`    // DDR3, DDR4, DDR5
	MinSpeedMHz    int      `json:"min_speed_mhz"`    // Minimum speed
	RequireECC     bool     `json:"require_ecc"`      // Require ECC memory
	AllowedSizes   []int    `json:"allowed_sizes_gb"` // Allowed module sizes in GB
	RequiredSlots  []string `json:"required_slots"`   // Specific slots that must be populated
	UniformModules bool     `json:"uniform_modules"`  // All modules must be identical
}

type RAMVisual struct {
	Symbol      string `json:"symbol"`
	ShortName   string `json:"short_name"`
	Description string `json:"description"`
	Color       string `json:"color"`
}

type RowConfig struct {
	Name  string `json:"name"`  // Display name for the row (e.g., "Memory Bank 1")
	Slots string `json:"slots"` // Slot range (e.g., "1-8", "9-24")
}

type CustomRowsConfig struct {
	Enabled bool        `json:"enabled"` // Enable custom row configuration
	Rows    []RowConfig `json:"rows"`    // Custom row definitions
}

type VisualizationConfig struct {
	TypeVisuals map[string]RAMVisual `json:"type_visuals"`  // memory type -> visual
	SlotMapping map[string]int       `json:"slot_mapping"`  // slot name -> logical position
	TotalSlots  int                  `json:"total_slots"`   // Total memory slots
	SlotWidth   int                  `json:"slot_width"`    // Width of each slot in visualization
	SlotsPerRow int                  `json:"slots_per_row"` // Number of slots per row (legacy)
	CustomRows  CustomRowsConfig     `json:"custom_rows"`   // Custom row configuration
}

type Config struct {
	RAMRequirements []RAMRequirement    `json:"ram_requirements"`
	Visualization   VisualizationConfig `json:"visualization"`
	CheckECC        bool                `json:"check_ecc"`
	CheckSpeed      bool                `json:"check_speed"`
	CheckUniform    bool                `json:"check_uniform_modules"`
}

type RAMCheckResult struct {
	Status      string // "ok", "warning", "error", "missing"
	Issues      []string
	SizeOK      bool
	SpeedOK     bool
	TypeOK      bool
	ECCOK       bool
	UniformOK   bool
	SizeWarn    bool
	SpeedWarn   bool
	TypeWarn    bool
	ECCWarn     bool
	UniformWarn bool
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
	fmt.Printf("RAM Checker %s\n", VERSION)
	fmt.Println("Parameters:")
	fmt.Println("  -V          Show program version")
	fmt.Println("  -c <path>   Path to configuration file")
	fmt.Println("  -s          Create default configuration file")
	fmt.Println("  -l          List detected RAM modules without configuration check")
	fmt.Println("  -vis        Show visual memory slots layout")
	fmt.Println("  -d          Show detailed debug information")
	fmt.Println("  -h          Show this help")
}

func getRAMInfo() ([]RAMInfo, error) {
	// Try dmidecode first (requires root)
	if ramsDMI, err := getRAMInfoDMIDecode(); err == nil && len(ramsDMI) > 0 {
		printDebug("Using dmidecode for detailed RAM information")
		return ramsDMI, nil
	} else {
		printDebug(fmt.Sprintf("dmidecode failed or returned no data: %v", err))
	}

	// Fallback to /proc/meminfo + lshw
	if ramsLSHW, err := getRAMInfoLSHW(); err == nil && len(ramsLSHW) > 0 {
		printDebug("Using lshw for RAM information")
		return ramsLSHW, nil
	} else {
		printDebug(fmt.Sprintf("lshw failed: %v", err))
	}

	// Final fallback to /proc/meminfo only
	if ramsProcOnly, err := getRAMInfoProcOnly(); err == nil && len(ramsProcOnly) > 0 {
		printWarning("Using /proc/meminfo only - limited information available")
		printWarning("For detailed module information, run as root or install dmidecode/lshw")
		return ramsProcOnly, nil
	}

	return nil, fmt.Errorf("failed to get RAM information from any source")
}

func getRAMInfoDMIDecode() ([]RAMInfo, error) {
	// Check if dmidecode is available
	if _, err := exec.LookPath("dmidecode"); err != nil {
		return nil, fmt.Errorf("dmidecode not found")
	}

	cmd := exec.Command("dmidecode", "-t", "memory")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("dmidecode failed: %v", err)
	}

	if debugMode {
		printDebug("dmidecode output:")
		printDebug(string(output))
		printDebug("--- End of dmidecode output ---")
	}

	return parseDMIDecodeOutput(string(output))
}

func parseDMIDecodeOutput(output string) ([]RAMInfo, error) {
	var rams []RAMInfo

	// Split into memory device sections
	sections := strings.Split(output, "Memory Device")

	for i, section := range sections {
		if i == 0 {
			continue // Skip header
		}

		ram := parseMemoryDeviceSection(section)
		rams = append(rams, ram)
	}

	if debugMode {
		printDebug(fmt.Sprintf("Parsed %d memory devices from dmidecode", len(rams)))
	}

	return rams, nil
}

func parseMemoryDeviceSection(section string) RAMInfo {
	var ram RAMInfo

	lines := strings.Split(section, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, ":") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch key {
		case "Locator":
			ram.Slot = value
		case "Size":
			ram.SizeMB = parseMemorySize(value)
			ram.IsPresent = ram.SizeMB > 0
		case "Type":
			ram.Type = value
		case "Speed":
			ram.Speed = parseMemorySpeed(value)
		case "Manufacturer":
			ram.Manufacturer = value
		case "Part Number":
			ram.PartNumber = value
		case "Serial Number":
			ram.SerialNumber = value
		case "Bank Locator":
			ram.Bank = value
		case "Type Detail":
			ram.IsECC = strings.Contains(strings.ToLower(value), "ecc")
		case "Configured Memory Speed", "Configured Clock Speed":
			if configSpeed := parseMemorySpeed(value); configSpeed > 0 {
				ram.Speed = configSpeed // Use configured speed if available
			}
		case "Voltage":
			ram.Voltage = parseVoltage(value)
		}
	}

	return ram
}

func parseMemorySize(sizeStr string) int {
	if sizeStr == "No Module Installed" || sizeStr == "Not Present" || sizeStr == "" {
		return 0
	}

	// Parse sizes like "8192 MB", "8 GB", "16384 MB"
	re := regexp.MustCompile(`(\d+)\s*(MB|GB|TB)`)
	matches := re.FindStringSubmatch(sizeStr)

	if len(matches) < 3 {
		return 0
	}

	size, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0
	}

	unit := strings.ToUpper(matches[2])
	switch unit {
	case "MB":
		return size
	case "GB":
		return size * 1024
	case "TB":
		return size * 1024 * 1024
	default:
		return 0
	}
}

func parseMemorySpeed(speedStr string) int {
	if speedStr == "Unknown" || speedStr == "" {
		return 0
	}

	// Parse speeds like "1600 MHz", "DDR4-2400", "2133 MT/s"
	re := regexp.MustCompile(`(\d+)\s*(?:MHz|MT/s)?`)
	matches := re.FindStringSubmatch(speedStr)

	if len(matches) < 2 {
		return 0
	}

	speed, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0
	}

	return speed
}

func parseVoltage(voltageStr string) float64 {
	if voltageStr == "Unknown" || voltageStr == "" {
		return 0
	}

	// Parse voltages like "1.2 V", "1.35V"
	re := regexp.MustCompile(`(\d+\.?\d*)\s*V`)
	matches := re.FindStringSubmatch(voltageStr)

	if len(matches) < 2 {
		return 0
	}

	voltage, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0
	}

	return voltage
}

func getRAMInfoLSHW() ([]RAMInfo, error) {
	// Check if lshw is available
	if _, err := exec.LookPath("lshw"); err != nil {
		return nil, fmt.Errorf("lshw not found")
	}

	cmd := exec.Command("lshw", "-c", "memory", "-short")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("lshw failed: %v", err)
	}

	if debugMode {
		printDebug("lshw output:")
		printDebug(string(output))
		printDebug("--- End of lshw output ---")
	}

	return parseLSHWOutput(string(output))
}

func parseLSHWOutput(output string) ([]RAMInfo, error) {
	var rams []RAMInfo
	lines := strings.Split(output, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "H/W path") {
			continue
		}

		// Parse lshw short format
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}

		if strings.Contains(strings.ToLower(line), "memory") {
			ram := RAMInfo{
				Slot:      "Unknown",
				IsPresent: true,
			}

			// Try to extract size from description
			for _, field := range fields {
				if size := parseMemorySize(field); size > 0 {
					ram.SizeMB = size
					break
				}
			}

			if ram.SizeMB > 0 {
				rams = append(rams, ram)
			}
		}
	}

	return rams, nil
}

func getRAMInfoProcOnly() ([]RAMInfo, error) {
	// Read total memory from /proc/meminfo
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, fmt.Errorf("failed to open /proc/meminfo: %v", err)
	}
	defer file.Close()

	var totalKB int
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if total, err := strconv.Atoi(fields[1]); err == nil {
					totalKB = total
					break
				}
			}
		}
	}

	if totalKB == 0 {
		return nil, fmt.Errorf("could not parse total memory from /proc/meminfo")
	}

	totalMB := totalKB / 1024

	// Create a single virtual module representing all memory
	ram := RAMInfo{
		Slot:      "System",
		SizeMB:    totalMB,
		Type:      "Unknown",
		IsPresent: true,
	}

	return []RAMInfo{ram}, nil
}

func parseSlotRange(rangeStr string) (int, int, error) {
	// Parse slot range like "1-8", "9-24", "25-32"
	parts := strings.Split(rangeStr, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid range format: %s (expected format: 'start-end')", rangeStr)
	}

	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid start number in range %s: %v", rangeStr, err)
	}

	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid end number in range %s: %v", rangeStr, err)
	}

	if start > end {
		return 0, 0, fmt.Errorf("invalid range %s: start (%d) cannot be greater than end (%d)", rangeStr, start, end)
	}

	return start, end, nil
}

func generateDefaultCustomRows(totalSlots int) CustomRowsConfig {
	// Generate reasonable default rows based on total slots
	var rows []RowConfig

	if totalSlots <= 8 {
		// Single row for small configurations
		rows = append(rows, RowConfig{
			Name:  "Memory Bank 1",
			Slots: fmt.Sprintf("1-%d", totalSlots),
		})
	} else if totalSlots <= 16 {
		// Two rows of 8 each
		rows = append(rows, RowConfig{
			Name:  "Memory Bank 1",
			Slots: "1-8",
		})
		rows = append(rows, RowConfig{
			Name:  "Memory Bank 2",
			Slots: fmt.Sprintf("9-%d", totalSlots),
		})
	} else if totalSlots <= 32 {
		// Multiple rows, try to keep them reasonably sized
		mid := totalSlots / 2
		rows = append(rows, RowConfig{
			Name:  "Memory Bank 1",
			Slots: fmt.Sprintf("1-%d", mid),
		})
		rows = append(rows, RowConfig{
			Name:  "Memory Bank 2",
			Slots: fmt.Sprintf("%d-%d", mid+1, totalSlots),
		})
	} else {
		// Large configurations: break into multiple rows
		slotsPerRow := 16 // Reasonable width for large configurations
		for i := 0; i < totalSlots; i += slotsPerRow {
			end := i + slotsPerRow
			if end > totalSlots {
				end = totalSlots
			}
			rows = append(rows, RowConfig{
				Name:  fmt.Sprintf("Memory Bank %d", len(rows)+1),
				Slots: fmt.Sprintf("%d-%d", i+1, end),
			})
		}
	}

	return CustomRowsConfig{
		Enabled: false, // Disabled by default for backward compatibility
		Rows:    rows,
	}
}

func createDefaultConfig(configPath string) error {
	printInfo("Scanning system for RAM information to create configuration...")

	rams, err := getRAMInfo()
	if err != nil {
		return fmt.Errorf("could not scan RAM: %v", err)
	}

	if len(rams) == 0 {
		return fmt.Errorf("no RAM modules found - cannot create configuration")
	}

	printInfo(fmt.Sprintf("Found %d memory slot(s), creating configuration:", len(rams)))

	totalSizeMB := 0
	installedModules := 0
	var memoryTypes []string
	var memorySpeeds []int
	hasECC := false
	slotMapping := make(map[string]int)

	for i, ram := range rams {
		if ram.IsPresent && ram.SizeMB > 0 {
			installedModules++
			totalSizeMB += ram.SizeMB

			printInfo(fmt.Sprintf("  Slot %s: %s %d MB", ram.Slot, ram.Type, ram.SizeMB))
			if ram.Speed > 0 {
				printInfo(fmt.Sprintf("    Speed: %d MHz", ram.Speed))
				memorySpeeds = append(memorySpeeds, ram.Speed)
			}
			if ram.Manufacturer != "" {
				printInfo(fmt.Sprintf("    Manufacturer: %s", ram.Manufacturer))
			}
			if ram.IsECC {
				printInfo(fmt.Sprintf("    ECC: Yes"))
				hasECC = true
			}

			if ram.Type != "" && ram.Type != "Unknown" {
				memoryTypes = append(memoryTypes, ram.Type)
			}
		} else {
			printInfo(fmt.Sprintf("  Slot %s: Empty", ram.Slot))
		}

		// Create slot mapping
		slotMapping[ram.Slot] = i + 1
	}

	totalSizeGB := totalSizeMB / 1024
	printInfo(fmt.Sprintf("Total installed memory: %d GB (%d MB)", totalSizeGB, totalSizeMB))

	// Find most common memory type
	commonType := findMostCommonString(memoryTypes)
	if commonType == "" {
		commonType = "DDR4" // Default assumption
	}

	// Find minimum speed
	minSpeed := 0
	if len(memorySpeeds) > 0 {
		minSpeed = memorySpeeds[0]
		for _, speed := range memorySpeeds {
			if speed < minSpeed {
				minSpeed = speed
			}
		}
	}

	// Create requirements
	var requirements []RAMRequirement

	req := RAMRequirement{
		Name:           fmt.Sprintf("Server Memory (%d modules, %d GB total)", installedModules, totalSizeGB),
		MinTotalSizeGB: totalSizeGB,
		MinModules:     installedModules,
		RequiredType:   commonType,
		MinSpeedMHz:    minSpeed,
		RequireECC:     hasECC,
		UniformModules: true, // Assume uniform modules are preferred
	}

	// Add specific slots that should be populated
	for _, ram := range rams {
		if ram.IsPresent && ram.SizeMB > 0 {
			req.RequiredSlots = append(req.RequiredSlots, ram.Slot)
		}
	}

	requirements = append(requirements, req)

	// Create type visuals
	typeVisuals := make(map[string]RAMVisual)

	for _, memType := range []string{"DDR3", "DDR4", "DDR5", "Unknown"} {
		visual := generateRAMVisual(memType, hasECC)
		typeVisuals[memType] = visual
	}

	config := Config{
		RAMRequirements: requirements,
		Visualization: VisualizationConfig{
			TypeVisuals: typeVisuals,
			SlotMapping: slotMapping,
			TotalSlots:  len(rams), // Use actual slot count, no extra slots
			SlotWidth:   10,
			SlotsPerRow: 8, // Legacy fallback
			CustomRows:  generateDefaultCustomRows(len(rams)),
		},
		CheckECC:     hasECC,       // Enable ECC checking if ECC memory detected
		CheckSpeed:   minSpeed > 0, // Enable speed checking if speed detected
		CheckUniform: true,
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
	printInfo(fmt.Sprintf("Total memory slots: %d", len(rams)))
	printInfo(fmt.Sprintf("Installed modules: %d", installedModules))
	printInfo(fmt.Sprintf("Total capacity: %d GB", totalSizeGB))
	if hasECC {
		printInfo("ECC checking enabled (ECC memory detected)")
	}
	if minSpeed > 0 {
		printInfo(fmt.Sprintf("Speed checking enabled (minimum: %d MHz)", minSpeed))
	}
	printInfo("Custom row layout generated (disabled by default)")
	printInfo("To enable custom rows: set 'visualization.custom_rows.enabled' to true")
	printInfo("You can edit the configuration file to adjust requirements and row layout as needed")

	return nil
}

func findMostCommonString(strings []string) string {
	if len(strings) == 0 {
		return ""
	}

	counts := make(map[string]int)
	for _, s := range strings {
		counts[s]++
	}

	maxCount := 0
	var mostCommon string
	for s, count := range counts {
		if count > maxCount {
			maxCount = count
			mostCommon = s
		}
	}

	return mostCommon
}

func generateRAMVisual(memType string, isECC bool) RAMVisual {
	visual := RAMVisual{
		Description: fmt.Sprintf("%s Memory", memType),
		Color:       "green",
	}

	symbol := "████"
	shortName := memType

	if isECC {
		symbol = "▓▓▓▓"
		shortName = shortName + "E"
	}

	switch memType {
	case "DDR3":
		visual.Symbol = symbol
		visual.ShortName = shortName
	case "DDR4":
		visual.Symbol = symbol
		visual.ShortName = shortName
	case "DDR5":
		visual.Symbol = symbol
		visual.ShortName = shortName
	default:
		visual.Symbol = "░░░░"
		visual.ShortName = "UNK"
	}

	return visual
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

func checkRAMAgainstRequirements(rams []RAMInfo, config *Config) RAMCheckResult {
	result := RAMCheckResult{
		Status:    "ok",
		SizeOK:    true,
		SpeedOK:   true,
		TypeOK:    true,
		ECCOK:     true,
		UniformOK: true,
	}

	if len(config.RAMRequirements) == 0 {
		return result
	}

	hasErrors := false
	hasWarnings := false

	// Calculate totals
	installedModules := 0
	totalSizeMB := 0
	var types []string
	var speeds []int
	var sizes []int
	hasECC := false

	for _, ram := range rams {
		if ram.IsPresent && ram.SizeMB > 0 {
			installedModules++
			totalSizeMB += ram.SizeMB

			if ram.Type != "" && ram.Type != "Unknown" {
				types = append(types, ram.Type)
			}
			if ram.Speed > 0 {
				speeds = append(speeds, ram.Speed)
			}
			sizes = append(sizes, ram.SizeMB)
			if ram.IsECC {
				hasECC = true
			}
		}
	}

	totalSizeGB := totalSizeMB / 1024

	for _, req := range config.RAMRequirements {
		// Check total size
		if req.MinTotalSizeGB > 0 && totalSizeGB < req.MinTotalSizeGB {
			result.Issues = append(result.Issues,
				fmt.Sprintf("Total memory: %d GB (required %d GB)", totalSizeGB, req.MinTotalSizeGB))
			result.SizeOK = false
			hasErrors = true
		}

		if req.MaxTotalSizeGB > 0 && totalSizeGB > req.MaxTotalSizeGB {
			result.Issues = append(result.Issues,
				fmt.Sprintf("Total memory: %d GB (maximum %d GB)", totalSizeGB, req.MaxTotalSizeGB))
			result.SizeOK = false
			hasErrors = true
		}

		// Check module count
		if req.MinModules > 0 && installedModules < req.MinModules {
			result.Issues = append(result.Issues,
				fmt.Sprintf("Installed modules: %d (required %d)", installedModules, req.MinModules))
			hasErrors = true
		}

		if req.MaxModules > 0 && installedModules > req.MaxModules {
			result.Issues = append(result.Issues,
				fmt.Sprintf("Installed modules: %d (maximum %d)", installedModules, req.MaxModules))
			hasErrors = true
		}

		// Check memory type
		if req.RequiredType != "" && req.RequiredType != "any" {
			typeMatches := 0
			for _, ramType := range types {
				if ramType == req.RequiredType {
					typeMatches++
				}
			}
			if typeMatches == 0 && len(types) > 0 {
				result.Issues = append(result.Issues,
					fmt.Sprintf("Memory type: %s (required %s)", types[0], req.RequiredType))
				result.TypeOK = false
				hasErrors = true
			}
		}

		// Check speed (if enabled)
		if config.CheckSpeed && req.MinSpeedMHz > 0 {
			if len(speeds) == 0 {
				result.Issues = append(result.Issues, "Could not determine memory speed")
				result.SpeedWarn = true
				hasWarnings = true
			} else {
				minSpeed := speeds[0]
				for _, speed := range speeds {
					if speed < minSpeed {
						minSpeed = speed
					}
				}
				if minSpeed < req.MinSpeedMHz {
					result.Issues = append(result.Issues,
						fmt.Sprintf("Memory speed: %d MHz (required %d MHz)", minSpeed, req.MinSpeedMHz))
					result.SpeedOK = false
					hasErrors = true
				}
			}
		}

		// Check ECC (if enabled)
		if config.CheckECC && req.RequireECC && !hasECC {
			result.Issues = append(result.Issues, "ECC memory required but not detected")
			result.ECCOK = false
			hasErrors = true
		}

		// Check uniform modules (if enabled)
		if config.CheckUniform && req.UniformModules && len(sizes) > 1 {
			firstSize := sizes[0]
			uniform := true
			for _, size := range sizes {
				if size != firstSize {
					uniform = false
					break
				}
			}
			if !uniform {
				result.Issues = append(result.Issues, "Memory modules have different sizes")
				result.UniformOK = false
				hasWarnings = true
			}
		}

		// Check required slots
		for _, reqSlot := range req.RequiredSlots {
			found := false
			for _, ram := range rams {
				if ram.Slot == reqSlot && ram.IsPresent && ram.SizeMB > 0 {
					found = true
					break
				}
			}
			if !found {
				result.Issues = append(result.Issues, fmt.Sprintf("Required slot %s is empty", reqSlot))
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

func formatSize(sizeMB int) string {
	if sizeMB == 0 {
		return "Empty"
	}
	if sizeMB < 1024 {
		return fmt.Sprintf("%dMB", sizeMB)
	} else {
		return fmt.Sprintf("%dGB", sizeMB/1024)
	}
}

func formatSpeed(speedMHz int) string {
	if speedMHz == 0 {
		return "?"
	}
	return fmt.Sprintf("%dMHz", speedMHz)
}

func shortenSlotName(slotName string) string {
	if slotName == "" || slotName == "System" {
		return slotName
	}

	// Parse different slot name formats and create short versions
	// Examples:
	// "Controller0-ChannelA" -> "C0A"
	// "ChannelA-DIMM0" -> "A0"
	// "ChannelE-DIMM1" -> "E1"
	// "DIMM_A1" -> "A1"
	// "DIMM1" -> "1"

	var result string

	// Extract Controller number
	controllerRegex := regexp.MustCompile(`Controller(\d+)`)
	if matches := controllerRegex.FindStringSubmatch(slotName); len(matches) > 1 {
		result += "C" + matches[1]
	}

	// Extract Channel letter
	channelRegex := regexp.MustCompile(`Channel([A-Z])`)
	if matches := channelRegex.FindStringSubmatch(slotName); len(matches) > 1 {
		result += matches[1]
	}

	// Extract DIMM number
	dimmRegex := regexp.MustCompile(`DIMM[_-]?([A-Z]?\d+)`)
	if matches := dimmRegex.FindStringSubmatch(slotName); len(matches) > 1 {
		result += matches[1]
	} else {
		// Try to extract just number at the end
		numberRegex := regexp.MustCompile(`(\d+)$`)
		if matches := numberRegex.FindStringSubmatch(slotName); len(matches) > 1 {
			result += matches[1]
		}
	}

	// If we couldn't parse anything meaningful, try some fallbacks
	if result == "" {
		// Try to extract letter-number patterns like "A1", "B2"
		letterNumberRegex := regexp.MustCompile(`([A-Z])(\d+)`)
		if matches := letterNumberRegex.FindStringSubmatch(slotName); len(matches) > 2 {
			result = matches[1] + matches[2]
		} else {
			// Last resort: take first 4 characters
			if len(slotName) > 4 {
				result = slotName[:4]
			} else {
				result = slotName
			}
		}
	}

	// Ensure result is not too long for visualization
	if len(result) > 6 {
		result = result[:6]
	}

	if debugMode {
		printDebug(fmt.Sprintf("Shortened slot name: '%s' -> '%s'", slotName, result))
	}

	return result
}

func visualizeSlots(rams []RAMInfo, config *Config) error {
	printInfo("Memory Slots Layout:")
	fmt.Println()

	maxSlots := config.Visualization.TotalSlots
	if maxSlots == 0 {
		maxSlots = len(rams)
	}

	// Create slot data array
	slotData := make([]RAMInfo, maxSlots+1) // +1 because slots start from 1
	slotResults := make([]RAMCheckResult, maxSlots+1)

	// Fill slots based on mapping
	expectedSlots := make(map[int]bool)
	for _, ram := range rams {
		if logicalSlot, exists := config.Visualization.SlotMapping[ram.Slot]; exists {
			if logicalSlot > 0 && logicalSlot <= maxSlots {
				slotData[logicalSlot] = ram
				expectedSlots[logicalSlot] = true
			}
		}
	}

	// Check overall system requirements
	systemResult := checkRAMAgainstRequirements(rams, config)

	// Set individual slot results
	for i := 1; i <= maxSlots; i++ {
		if slotData[i].IsPresent {
			slotResults[i] = RAMCheckResult{Status: "ok"}
			if systemResult.Status == "error" {
				slotResults[i].Status = "warning" // Individual modules OK but system has issues
			}
		} else {
			slotResults[i] = RAMCheckResult{Status: "empty"}
		}
	}

	// Count status types
	hasErrors := systemResult.Status == "error"
	hasWarnings := systemResult.Status == "warning"

	// Print legend
	printInfo("Legend:")
	fmt.Printf("  %s%s%s Memory Module Present  ", ColorGreen, "████", ColorReset)
	fmt.Printf("  %s%s%s Module with Issues  ", ColorYellow, "████", ColorReset)
	fmt.Printf("  %s%s%s Missing Module  ", ColorRed, "░░░░", ColorReset)
	fmt.Printf("  %s%s%s Empty Slot\n", ColorWhite, "░░░░", ColorReset)
	fmt.Println()

	// Report system-level issues
	if len(systemResult.Issues) > 0 {
		if systemResult.Status == "error" {
			printError("Memory configuration issues:")
		} else if systemResult.Status == "warning" {
			printWarning("Memory configuration warnings:")
		}

		for _, issue := range systemResult.Issues {
			if systemResult.Status == "error" {
				printError(fmt.Sprintf("  - %s", issue))
			} else if systemResult.Status == "warning" {
				printWarning(fmt.Sprintf("  - %s", issue))
			}
		}
		fmt.Println()
	}

	// Calculate rows configuration
	var rowsConfig []RowConfig
	if config.Visualization.CustomRows.Enabled && len(config.Visualization.CustomRows.Rows) > 0 {
		// Use custom rows configuration
		rowsConfig = config.Visualization.CustomRows.Rows
		printDebug("Using custom rows configuration")
	} else {
		// Use legacy SlotsPerRow configuration
		slotsPerRow := config.Visualization.SlotsPerRow
		if slotsPerRow == 0 {
			slotsPerRow = 8
		}

		// Generate legacy rows
		for i := 0; i < maxSlots; i += slotsPerRow {
			end := i + slotsPerRow
			if end > maxSlots {
				end = maxSlots
			}
			rowsConfig = append(rowsConfig, RowConfig{
				Name:  fmt.Sprintf("Memory Bank %d", len(rowsConfig)+1),
				Slots: fmt.Sprintf("%d-%d", i+1, end),
			})
		}
		printDebug("Using legacy SlotsPerRow configuration")
	}

	// Process each row according to configuration
	for rowIndex, rowConfig := range rowsConfig {
		// Parse slot range for this row
		startSlot, endSlot, err := parseSlotRange(rowConfig.Slots)
		if err != nil {
			printError(fmt.Sprintf("Invalid slot range in row %d (%s): %v", rowIndex+1, rowConfig.Name, err))
			continue
		}

		// Ensure slots are within bounds
		if startSlot > maxSlots || endSlot > maxSlots {
			printWarning(fmt.Sprintf("Row %d (%s) slot range %s exceeds available slots (%d)",
				rowIndex+1, rowConfig.Name, rowConfig.Slots, maxSlots))
			continue
		}

		rowSlots := endSlot - startSlot + 1

		if len(rowsConfig) > 1 {
			fmt.Printf("%s (Slots %d-%d):\n", rowConfig.Name, startSlot, endSlot)
		}

		// Build row visualization
		width := config.Visualization.SlotWidth

		// Top border
		fmt.Print("┌")
		for i := 0; i < rowSlots; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < rowSlots-1 {
				fmt.Print("┬")
			}
		}
		fmt.Println("┐")

		// Symbols row
		fmt.Print("│")
		for i := 0; i < rowSlots; i++ {
			slotIdx := startSlot + i
			visual := getRAMVisual(slotData[slotIdx], &config.Visualization)
			result := slotResults[slotIdx]

			symbolText := centerText(visual.Symbol, width)

			if slotData[slotIdx].IsPresent {
				switch result.Status {
				case "ok":
					fmt.Print(ColorGreen + symbolText + ColorReset)
				case "warning", "error":
					fmt.Print(ColorYellow + symbolText + ColorReset)
				default:
					fmt.Print(symbolText)
				}
			} else {
				fmt.Print(centerText("░░░░", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Type row
		fmt.Print("│")
		for i := 0; i < rowSlots; i++ {
			slotIdx := startSlot + i
			result := slotResults[slotIdx]

			if slotData[slotIdx].IsPresent {
				visual := getRAMVisual(slotData[slotIdx], &config.Visualization)
				typeText := centerText(visual.ShortName, width)

				switch result.Status {
				case "ok":
					fmt.Print(ColorGreen + typeText + ColorReset)
				case "warning", "error":
					fmt.Print(ColorYellow + typeText + ColorReset)
				default:
					fmt.Print(typeText)
				}
			} else {
				fmt.Print(strings.Repeat(" ", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Size row
		fmt.Print("│")
		for i := 0; i < rowSlots; i++ {
			slotIdx := startSlot + i
			result := slotResults[slotIdx]

			var sizeInfo string
			if slotData[slotIdx].IsPresent {
				sizeInfo = formatSize(slotData[slotIdx].SizeMB)
			} else {
				sizeInfo = ""
			}

			sizeText := centerText(sizeInfo, width)

			if slotData[slotIdx].IsPresent {
				switch result.Status {
				case "ok":
					fmt.Print(ColorGreen + sizeText + ColorReset)
				case "warning", "error":
					fmt.Print(ColorYellow + sizeText + ColorReset)
				default:
					fmt.Print(sizeText)
				}
			} else {
				fmt.Print(strings.Repeat(" ", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Speed row (if speed checking enabled)
		if config.CheckSpeed {
			fmt.Print("│")
			for i := 0; i < rowSlots; i++ {
				slotIdx := startSlot + i
				result := slotResults[slotIdx]

				var speedInfo string
				if slotData[slotIdx].IsPresent {
					speedInfo = formatSpeed(slotData[slotIdx].Speed)
				} else {
					speedInfo = ""
				}

				speedText := centerText(speedInfo, width)

				if slotData[slotIdx].IsPresent {
					switch result.Status {
					case "ok":
						fmt.Print(ColorGreen + speedText + ColorReset)
					case "warning", "error":
						fmt.Print(ColorYellow + speedText + ColorReset)
					default:
						fmt.Print(speedText)
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
		for i := 0; i < rowSlots; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < rowSlots-1 {
				fmt.Print("┴")
			}
		}
		fmt.Println("┘")

		// Slot numbers
		fmt.Print(" ")
		for i := 0; i < rowSlots; i++ {
			slotIdx := startSlot + i
			fmt.Print(centerText(fmt.Sprintf("%d", slotIdx), width+1))
		}
		fmt.Println("(Slot)")

		// Slot names with shortened names
		fmt.Print(" ")
		for i := 0; i < rowSlots; i++ {
			slotIdx := startSlot + i
			slotName := ""
			if slotData[slotIdx].Slot != "" {
				slotName = shortenSlotName(slotData[slotIdx].Slot)
			} else {
				slotName = "-"
			}
			fmt.Print(centerText(slotName, width+1))
		}
		fmt.Println("(Name)")

		fmt.Println()
	}

	// Final status
	if hasErrors {
		printError("Memory configuration validation FAILED!")
		return fmt.Errorf("memory configuration validation failed")
	} else if hasWarnings {
		printWarning("Memory configuration validation completed with warnings")
		return nil
	} else {
		printSuccess("All memory modules present and meet requirements!")
		return nil
	}
}

func getRAMVisual(ram RAMInfo, config *VisualizationConfig) RAMVisual {
	if !ram.IsPresent || ram.SizeMB == 0 {
		return RAMVisual{
			Symbol:      "░░░░",
			ShortName:   "",
			Description: "Empty Slot",
			Color:       "gray",
		}
	}

	memType := ram.Type
	if memType == "" {
		memType = "Unknown"
	}

	if visual, exists := config.TypeVisuals[memType]; exists {
		return visual
	}

	return generateRAMVisual(memType, ram.IsECC)
}

func checkRAM(config *Config) error {
	printInfo("Starting RAM check...")

	rams, err := getRAMInfo()
	if err != nil {
		return fmt.Errorf("failed to get RAM info: %v", err)
	}

	printInfo(fmt.Sprintf("Found memory slots: %d", len(rams)))

	if len(rams) == 0 {
		printError("No memory slots found")
		return fmt.Errorf("no memory slots found")
	}

	// Display found memory modules
	installedCount := 0
	totalSizeMB := 0

	for i, ram := range rams {
		if ram.IsPresent && ram.SizeMB > 0 {
			installedCount++
			totalSizeMB += ram.SizeMB
			printInfo(fmt.Sprintf("Slot %d (%s): %s %s", i+1, ram.Slot, formatSize(ram.SizeMB), ram.Type))
			if ram.Speed > 0 {
				printDebug(fmt.Sprintf("  Speed: %d MHz", ram.Speed))
			}
			if ram.Manufacturer != "" {
				printDebug(fmt.Sprintf("  Manufacturer: %s", ram.Manufacturer))
			}
			if ram.IsECC {
				printDebug(fmt.Sprintf("  ECC: Yes"))
			}
		} else {
			printInfo(fmt.Sprintf("Slot %d (%s): Empty", i+1, ram.Slot))
		}
	}

	totalSizeGB := totalSizeMB / 1024
	printInfo(fmt.Sprintf("Total installed: %d modules, %d GB", installedCount, totalSizeGB))

	// Check requirements
	result := checkRAMAgainstRequirements(rams, config)

	if len(result.Issues) > 0 {
		for _, issue := range result.Issues {
			if result.Status == "error" {
				printError(fmt.Sprintf("  %s", issue))
			} else if result.Status == "warning" {
				printWarning(fmt.Sprintf("  %s", issue))
			}
		}
	}

	if result.Status == "error" {
		printError("Memory requirements FAILED")
		return fmt.Errorf("memory requirements not met")
	} else if result.Status == "warning" {
		printWarning("Memory requirements passed with warnings")
	} else {
		printSuccess("All memory requirements passed")
	}

	return nil
}

func main() {
	var (
		showVersion  = flag.Bool("V", false, "Show version")
		configPath   = flag.String("c", "ram_config.json", "Path to configuration file")
		createConfig = flag.Bool("s", false, "Create default configuration file")
		showHelpFlag = flag.Bool("h", false, "Show help")
		listOnly     = flag.Bool("l", false, "List detected RAM modules without configuration check")
		visualize    = flag.Bool("vis", false, "Show visual memory slots layout")
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
		printInfo("Scanning for memory modules...")
		rams, err := getRAMInfo()
		if err != nil {
			printError(fmt.Sprintf("Error getting RAM information: %v", err))
			os.Exit(1)
		}

		if len(rams) == 0 {
			printWarning("No memory modules found")
		} else {
			printSuccess(fmt.Sprintf("Found memory slots: %d", len(rams)))
			installedCount := 0
			totalSizeMB := 0

			for i, ram := range rams {
				fmt.Printf("\nSlot %d:\n", i+1)
				fmt.Printf("  Slot Name: %s\n", ram.Slot)
				fmt.Printf("  Size: %s\n", formatSize(ram.SizeMB))
				fmt.Printf("  Type: %s\n", ram.Type)
				fmt.Printf("  Speed: %s\n", formatSpeed(ram.Speed))
				fmt.Printf("  Manufacturer: %s\n", ram.Manufacturer)
				fmt.Printf("  Part Number: %s\n", ram.PartNumber)
				fmt.Printf("  Bank: %s\n", ram.Bank)
				fmt.Printf("  ECC: %t\n", ram.IsECC)
				fmt.Printf("  Present: %t\n", ram.IsPresent)
				if ram.Voltage > 0 {
					fmt.Printf("  Voltage: %.2f V\n", ram.Voltage)
				}

				if ram.IsPresent && ram.SizeMB > 0 {
					installedCount++
					totalSizeMB += ram.SizeMB
				}
			}

			fmt.Printf("\nSummary:\n")
			fmt.Printf("  Total slots: %d\n", len(rams))
			fmt.Printf("  Installed modules: %d\n", installedCount)
			fmt.Printf("  Total capacity: %d GB\n", totalSizeMB/1024)
		}
		return
	}

	if *visualize {
		printInfo("Scanning for memory modules...")
		rams, err := getRAMInfo()
		if err != nil {
			printError(fmt.Sprintf("Error getting RAM information: %v", err))
			os.Exit(1)
		}

		config, err := loadConfig(*configPath)
		if err != nil {
			printError(fmt.Sprintf("Error loading configuration: %v", err))
			printInfo("Use -s to create a default configuration file")
			os.Exit(1)
		}

		err = visualizeSlots(rams, config)
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
		printInfo("Or use -l to simply display found memory modules")
		os.Exit(1)
	}

	printInfo(fmt.Sprintf("Configuration loaded from: %s", *configPath))

	err = checkRAM(config)
	if err != nil {
		printError(fmt.Sprintf("RAM check failed: %v", err))
		os.Exit(1)
	}
}
