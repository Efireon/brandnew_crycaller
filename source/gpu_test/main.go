package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const VERSION = "2.0.0"

type GPUInfo struct {
	Name         string `json:"name"`
	PCISlot      string `json:"pci_slot"`
	PhysicalSlot string `json:"physical_slot"`
	PCILines     int    `json:"pci_lines"`
	MemoryMB     int    `json:"memory_mb"`
	Driver       string `json:"driver"`
	Vendor       string `json:"vendor"`
	DeviceID     string `json:"device_id"`
}

type GPURequirement struct {
	Name        string   `json:"name"`
	MinPCILines int      `json:"min_pci_lines"`
	MinMemoryMB int      `json:"min_memory_mb"`
	Driver      string   `json:"driver"`
	Vendor      string   `json:"vendor"`
	DeviceIDs   []string `json:"device_ids,omitempty"`
	MinCount    int      `json:"min_count"`
}

// Структура для детальной информации о проверке GPU
type GPUCheckResult struct {
	Status       string // "ok", "warning", "error"
	Issues       []string
	PCILinesOK   bool
	MemoryOK     bool
	DriverOK     bool
	PCILinesWarn bool
	MemoryWarn   bool
	DriverWarn   bool
}

type DeviceVisual struct {
	Symbol      string `json:"symbol"`
	ShortName   string `json:"short_name"`
	Description string `json:"description"`
	Color       string `json:"color"`
}

type VisualizationConfig struct {
	DeviceMap  map[string]DeviceVisual `json:"device_map"`  // device_id -> visual
	PCIToSlot  map[string]int          `json:"pci_to_slot"` // PCI address -> logical slot number
	TotalSlots int                     `json:"total_slots"` // Total number of slots to show
	SlotWidth  int                     `json:"slot_width"`
}

type Config struct {
	GPURequirements []GPURequirement    `json:"gpu_requirements"`
	Visualization   VisualizationConfig `json:"visualization"`
	CheckPower      bool                `json:"check_power"`
}

// ANSI color codes
const (
	ColorReset  = "\033[0m"
	ColorGreen  = "\033[32m"
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
	printColored(ColorWhite, message)
}

func printWarning(message string) {
	printColored(ColorYellow, message)
}

func printError(message string) {
	printColored(ColorRed, message)
}

func showHelp() {
	fmt.Printf("GPU Checker %s\n", VERSION)
	fmt.Println("Parameters:")
	fmt.Println("  -V          Show program version")
	fmt.Println("  -c <path>   Path to configuration file")
	fmt.Println("  -s          Create default configuration file")
	fmt.Println("  -l          List detected GPUs without configuration check")
	fmt.Println("  -vis        Show visual PCIe slots layout")
	fmt.Println("  -d          Show detailed debug information")
	fmt.Println("  -h          Show this help")
}

func createDefaultConfig(configPath string) error {
	printInfo("Scanning system for GPU devices to create configuration...")

	// Get current GPU information
	gpus, err := getGPUInfo()
	if err != nil {
		return fmt.Errorf("could not scan GPUs: %v", err)
	}

	if len(gpus) == 0 {
		return fmt.Errorf("no GPU devices found in system - cannot create configuration")
	}

	printInfo(fmt.Sprintf("Found %d GPU device(s), creating configuration based on detected hardware:", len(gpus)))

	var requirements []GPURequirement
	deviceMap := make(map[string]DeviceVisual)
	pciToSlot := make(map[string]int)

	// Sort GPUs by Physical Slot if available, otherwise by PCI address
	sortedGPUs := make([]GPUInfo, len(gpus))
	copy(sortedGPUs, gpus)

	// Simple sorting by PCI address for now (can be improved to use Physical Slot)
	for i := 0; i < len(sortedGPUs)-1; i++ {
		for j := i + 1; j < len(sortedGPUs); j++ {
			if sortedGPUs[i].PCISlot > sortedGPUs[j].PCISlot {
				sortedGPUs[i], sortedGPUs[j] = sortedGPUs[j], sortedGPUs[i]
			}
		}
	}

	// Create PCI to slot mapping
	for i, gpu := range sortedGPUs {
		logicalSlot := i + 1
		pciToSlot[gpu.PCISlot] = logicalSlot
		printInfo(fmt.Sprintf("  Mapping PCI %s -> Logical Slot %d (Physical: %s)",
			gpu.PCISlot, logicalSlot, gpu.PhysicalSlot))
	}

	// Group devices by vendor and create requirements
	vendorGroups := make(map[string][]GPUInfo)
	for _, gpu := range gpus {
		vendorGroups[gpu.Vendor] = append(vendorGroups[gpu.Vendor], gpu)
	}

	for vendor, gpusOfVendor := range vendorGroups {
		printInfo(fmt.Sprintf("  Processing %d %s device(s):", len(gpusOfVendor), vendor))

		var deviceIDs []string
		minPCILines := 0
		minMemoryMB := 0
		driver := ""

		for _, gpu := range gpusOfVendor {
			printInfo(fmt.Sprintf("    - %s (Device ID: %s, PCI: %s, Physical: %s)",
				gpu.Name, gpu.DeviceID, gpu.PCISlot, gpu.PhysicalSlot))
			printInfo(fmt.Sprintf("      PCI Lines: %d, Memory: %d MB, Driver: %s",
				gpu.PCILines, gpu.MemoryMB, gpu.Driver))

			// Collect device IDs
			if gpu.DeviceID != "" {
				deviceIDs = append(deviceIDs, gpu.DeviceID)

				// Generate visual representation for this specific device
				visual := DeviceVisual{
					Symbol:      generateSymbol(gpu),
					ShortName:   generateShortName(gpu),
					Description: gpu.Name,
					Color:       generateColor(gpu.Name),
				}

				deviceMap[gpu.DeviceID] = visual
				printInfo(fmt.Sprintf("      Added Device ID %s -> Symbol: %s, Short: %s, Color: %s",
					gpu.DeviceID, visual.Symbol, visual.ShortName, visual.Color))
			} else {
				printWarning(fmt.Sprintf("      No Device ID found for device: %s", gpu.Name))
			}

			// Set minimum requirements based on detected values
			if gpu.PCILines > minPCILines {
				minPCILines = gpu.PCILines
			}
			if gpu.MemoryMB > minMemoryMB {
				minMemoryMB = gpu.MemoryMB
			}
			if driver == "" && gpu.Driver != "unknown" {
				driver = gpu.Driver
			}
		}

		// Create requirement for this vendor group
		req := GPURequirement{
			Name:        fmt.Sprintf("%s devices (minimum %d)", capitalizeVendor(vendor), len(gpusOfVendor)),
			Vendor:      vendor,
			DeviceIDs:   deviceIDs,
			MinCount:    len(gpusOfVendor),
			MinPCILines: minPCILines,
			MinMemoryMB: minMemoryMB,
			Driver:      driver,
		}

		requirements = append(requirements, req)
	}

	// Check if device map is empty
	if len(deviceMap) == 0 {
		printWarning("No Device IDs were extracted from any devices!")
		printWarning("This usually means lspci output format is unexpected.")
		printWarning("Try running with -d flag for debug output to see parsing details.")
		return fmt.Errorf("no device IDs found - cannot create meaningful configuration")
	}

	config := Config{
		GPURequirements: requirements,
		Visualization: VisualizationConfig{
			DeviceMap:  deviceMap,
			PCIToSlot:  pciToSlot,
			TotalSlots: len(gpus) + 2, // Found GPUs + 2 extra slots
			SlotWidth:  9,
		},
		CheckPower: true,
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	err = os.MkdirAll(filepath.Dir(configPath), 0755)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(configPath, data, 0644)
	if err != nil {
		return err
	}

	printSuccess("Configuration created successfully based on detected hardware")
	printInfo("All device mappings are based on real Device IDs found in your system")
	printInfo(fmt.Sprintf("PCI to slot mapping created for %d device(s)", len(pciToSlot)))
	printInfo(fmt.Sprintf("Total slots configured: %d", len(gpus)+2))
	printInfo("You can edit the configuration file to adjust requirements and visuals as needed")
	printInfo("  - Modify 'total_slots' to change number of displayed slots")
	printInfo("  - Edit 'pci_to_slot' mapping to rearrange device positions")

	return nil
}

func generateSymbol(gpu GPUInfo) string {
	// Generate symbol based on device type
	nameLower := strings.ToLower(gpu.Name)

	if strings.Contains(nameLower, "aspeed") || strings.Contains(nameLower, "bmc") {
		return "▒▒▒" // BMC devices
	} else if strings.Contains(nameLower, "network") || strings.Contains(nameLower, "ethernet") {
		return "═══" // Network devices
	} else if strings.Contains(nameLower, "storage") || strings.Contains(nameLower, "sata") || strings.Contains(nameLower, "nvme") {
		return "███" // Storage devices
	} else {
		return "▓▓▓" // GPU and other graphics devices
	}
}

func generateShortName(gpu GPUInfo) string {
	// Extract first 3 characters after "VGA compatible controller:" or similar
	name := gpu.Name

	// Remove common prefixes
	prefixes := []string{
		"VGA compatible controller: ",
		"3D controller: ",
		"Display controller: ",
	}

	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			name = strings.TrimPrefix(name, prefix)
			break
		}
	}

	// Get first word and take first 3 characters
	words := strings.Fields(name)
	if len(words) > 0 {
		firstWord := words[0]
		if len(firstWord) >= 3 {
			return strings.ToUpper(firstWord[:3])
		} else {
			return strings.ToUpper(firstWord)
		}
	}

	// Fallback to vendor short name
	return shortenVendorName(gpu.Vendor)
}

func generateColor(name string) string {
	// Generate color based on hash of name
	colors := []string{"red", "green", "blue", "yellow", "cyan", "magenta", "white"}

	// Simple hash function
	hash := 0
	for _, char := range name {
		hash = hash*31 + int(char)
	}
	if hash < 0 {
		hash = -hash
	}

	return colors[hash%len(colors)]
}

func capitalizeVendor(vendor string) string {
	switch strings.ToLower(vendor) {
	case "nvidia":
		return "NVIDIA"
	case "amd":
		return "AMD"
	case "intel":
		return "Intel"
	case "aspeed":
		return "ASPEED"
	default:
		return strings.Title(vendor)
	}
}

func shortenVendorName(vendor string) string {
	switch strings.ToLower(vendor) {
	case "nvidia":
		return "NV"
	case "intel":
		return "INT"
	case "amd":
		return "AMD"
	case "aspeed":
		return "ASP"
	case "mellanox":
		return "MLX"
	case "broadcom":
		return "BCM"
	case "unknown":
		return "UNK"
	default:
		if len(vendor) > 3 {
			return strings.ToUpper(vendor[:3])
		}
		return strings.ToUpper(vendor)
	}
}

func loadConfig(configPath string) (*Config, error) {
	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var config Config
	err = json.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}

	return &config, nil
}

func getGPUInfo() ([]GPUInfo, error) {
	var gpus []GPUInfo

	// Get detailed information via lspci -vv
	cmd := exec.Command("lspci", "-vv")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run lspci -vv: %v", err)
	}

	if debugMode {
		printDebug("Full lspci -vv output:")
		printDebug(string(output))
		printDebug("--- End of lspci output ---")
	}

	// Parse lspci output by device blocks
	blocks := strings.Split(string(output), "\n\n")

	for _, block := range blocks {
		if strings.TrimSpace(block) == "" {
			continue
		}

		lines := strings.Split(block, "\n")
		if len(lines) == 0 {
			continue
		}

		firstLine := lines[0]

		// Check if this is a GPU device
		if !isGPUDevice(firstLine) {
			continue
		}

		if debugMode {
			printDebug(fmt.Sprintf("Processing GPU block:\n%s", block))
		}

		gpu := parseGPUBlock(lines)
		if gpu.Name != "" {
			gpus = append(gpus, gpu)
			if debugMode {
				printDebug(fmt.Sprintf("Successfully parsed GPU: %+v", gpu))
			}
		}
	}

	return gpus, nil
}

func isGPUDevice(line string) bool {
	line = strings.ToLower(line)

	// Must contain VGA, 3D controller, or Display controller
	hasGPUKeyword := strings.Contains(line, "vga") ||
		strings.Contains(line, "3d controller") ||
		strings.Contains(line, "display controller")

	if !hasGPUKeyword {
		return false
	}

	// Exclude audio devices
	excludeKeywords := []string{
		"audio", "sound", "hdmi audio", "high definition audio",
	}

	for _, keyword := range excludeKeywords {
		if strings.Contains(line, keyword) {
			return false
		}
	}

	return true
}

func parseGPUBlock(lines []string) GPUInfo {
	var gpu GPUInfo

	firstLine := lines[0]

	// Parse PCI slot and basic info from first line
	// Format: 01:00.0 VGA compatible controller: NVIDIA Corporation GP104 [GeForce GTX 1070] (rev a1)
	pciRegex := regexp.MustCompile(`^([0-9a-f]+:[0-9a-f]+\.[0-9a-f]+)\s+.*?:\s*(.+?)$`)
	matches := pciRegex.FindStringSubmatch(firstLine)

	if len(matches) >= 3 {
		gpu.PCISlot = matches[1]
		fullName := strings.TrimSpace(matches[2])

		// Parse vendor and device ID from the name
		gpu.Name = fullName
		gpu.Vendor, gpu.DeviceID = parseVendorAndDeviceID(fullName)

		if debugMode {
			printDebug(fmt.Sprintf("Parsed first line: PCI=%s, Name=%s, Vendor=%s, DeviceID=%s",
				gpu.PCISlot, gpu.Name, gpu.Vendor, gpu.DeviceID))
		}
	}

	// If no device ID found yet, try to get it via lspci -n for this specific device
	if gpu.DeviceID == "" && gpu.PCISlot != "" {
		cmd := exec.Command("lspci", "-n", "-s", gpu.PCISlot)
		if output, err := cmd.Output(); err == nil {
			line := strings.TrimSpace(string(output))
			// Format: 01:00.0 0300: 10de:1b06 (rev a1)
			deviceRegex := regexp.MustCompile(`([0-9a-f]+:[0-9a-f]+\.[0-9a-f]+)\s+[0-9a-f]+:\s+([0-9a-f]+:[0-9a-f]+)`)
			if matches := deviceRegex.FindStringSubmatch(line); len(matches) >= 3 {
				gpu.DeviceID = matches[2]
				if debugMode {
					printDebug(fmt.Sprintf("Got Device ID from lspci -n: %s", gpu.DeviceID))
				}
			}
		}
	}

	// Parse detailed information from subsequent lines
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)

		// Parse kernel driver
		if strings.HasPrefix(line, "Kernel driver in use:") {
			gpu.Driver = strings.TrimSpace(strings.TrimPrefix(line, "Kernel driver in use:"))
		}

		// Parse Physical Slot
		if strings.HasPrefix(line, "Physical Slot:") {
			gpu.PhysicalSlot = strings.TrimSpace(strings.TrimPrefix(line, "Physical Slot:"))
		}

		// Parse subsystem information for better vendor detection
		if strings.HasPrefix(line, "Subsystem:") {
			subsystem := strings.TrimSpace(strings.TrimPrefix(line, "Subsystem:"))
			if gpu.Vendor == "unknown" {
				gpu.Vendor, _ = parseVendorAndDeviceID(subsystem)
			}
		}

		// Parse PCI capabilities for ACTIVE lane information (LnkSta, not LnkCap)
		if strings.Contains(line, "LnkSta:") {
			gpu.PCILines = parsePCILanes(line)
		}

		// Parse memory regions
		if strings.HasPrefix(line, "Memory at") {
			memSize := parseMemoryRegion(line)
			if memSize > gpu.MemoryMB {
				gpu.MemoryMB = memSize
			}
		}

		// Parse prefetchable memory (usually VRAM for GPUs)
		if strings.Contains(line, "prefetchable") && strings.Contains(line, "Memory at") {
			memSize := parseMemoryRegion(line)
			if memSize > gpu.MemoryMB {
				gpu.MemoryMB = memSize
			}
		}
	}

	// Set defaults for missing fields
	if gpu.Driver == "" {
		gpu.Driver = "unknown"
	}
	if gpu.Vendor == "" {
		gpu.Vendor = "unknown"
	}
	if gpu.PhysicalSlot == "" {
		gpu.PhysicalSlot = "unknown"
	}

	return gpu
}

func parseVendorAndDeviceID(name string) (vendor, deviceID string) {
	nameLower := strings.ToLower(name)

	// Extract device ID if present [1234:5678]
	deviceRegex := regexp.MustCompile(`\[([0-9a-f]+:[0-9a-f]+)\]`)
	if matches := deviceRegex.FindStringSubmatch(name); len(matches) >= 2 {
		deviceID = matches[1]
	}

	// If no device ID found in brackets, try to extract from other patterns
	if deviceID == "" {
		// Try to find vendor:device pattern without brackets
		deviceRegex2 := regexp.MustCompile(`([0-9a-f]{4}):([0-9a-f]{4})`)
		if matches := deviceRegex2.FindStringSubmatch(name); len(matches) >= 3 {
			deviceID = matches[1] + ":" + matches[2]
		}
	}

	// Determine vendor
	if strings.Contains(nameLower, "nvidia") {
		vendor = "nvidia"
	} else if strings.Contains(nameLower, "amd") || strings.Contains(nameLower, "ati") {
		vendor = "amd"
	} else if strings.Contains(nameLower, "intel") {
		vendor = "intel"
	} else if strings.Contains(nameLower, "aspeed") {
		vendor = "aspeed"
	} else {
		vendor = "unknown"
	}

	return vendor, deviceID
}

func parsePCILanes(line string) int {
	// Look for "Width x16" or similar in LnkSta line (ACTIVE lanes)
	// Example: LnkSta: Speed 8GT/s, Width x16, TrErr- Train- SlotClk+ DLActive+ BWMgmt+ ABWMgmt-
	widthRegex := regexp.MustCompile(`Width\s+x(\d+)`)
	if matches := widthRegex.FindStringSubmatch(line); len(matches) >= 2 {
		if width, err := strconv.Atoi(matches[1]); err == nil {
			return width
		}
	}
	return 0
}

func parseMemoryRegion(line string) int {
	// Parse memory size from lines like:
	// Memory at f6000000 (32-bit, non-prefetchable) [size=16M]
	// Memory at e0000000 (64-bit, prefetchable) [size=256M]

	sizeRegex := regexp.MustCompile(`\[size=(\d+)([KMGT]?)\]`)
	matches := sizeRegex.FindStringSubmatch(line)

	if len(matches) < 2 {
		return 0
	}

	size, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0
	}

	unit := "B"
	if len(matches) > 2 && matches[2] != "" {
		unit = matches[2]
	}

	switch unit {
	case "K":
		return size / 1024 // KB to MB
	case "M":
		return size // MB
	case "G":
		return size * 1024 // GB to MB
	case "T":
		return size * 1024 * 1024 // TB to MB
	default:
		// Assume bytes, convert to MB
		if size >= 1024*1024 {
			return size / (1024 * 1024)
		}
		return 0
	}
}

func centerText(text string, width int) string {
	if len(text) == 0 {
		return strings.Repeat(" ", width)
	}

	// Use runes to properly handle UTF-8
	runes := []rune(text)
	runeLen := len(runes)

	if runeLen > width {
		// Simple truncation without problematic characters
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

func formatMemory(memoryMB int) string {
	if memoryMB == 0 {
		return "?"
	}

	if memoryMB < 1024 {
		return fmt.Sprintf("%dMB", memoryMB)
	} else if memoryMB < 1024*1024 {
		gb := float64(memoryMB) / 1024.0
		if gb == float64(int(gb)) {
			return fmt.Sprintf("%dGB", int(gb))
		} else {
			return fmt.Sprintf("%.1fGB", gb)
		}
	} else {
		tb := float64(memoryMB) / (1024.0 * 1024.0)
		if tb == float64(int(tb)) {
			return fmt.Sprintf("%dTB", int(tb))
		} else {
			return fmt.Sprintf("%.1fTB", tb)
		}
	}
}

// Новая функция для проверки GPU против требований
func checkGPUAgainstRequirements(gpu GPUInfo, requirements []GPURequirement) GPUCheckResult {
	result := GPUCheckResult{
		Status:     "ok",
		PCILinesOK: true,
		MemoryOK:   true,
		DriverOK:   true,
	}

	// Find matching requirements for this GPU
	var matchingReqs []GPURequirement

	for _, req := range requirements {
		// Check if this GPU matches the requirement
		if len(req.DeviceIDs) > 0 {
			// Check specific device IDs
			for _, deviceID := range req.DeviceIDs {
				if gpu.DeviceID == deviceID {
					matchingReqs = append(matchingReqs, req)
					break
				}
			}
		} else if req.Vendor != "" && req.Vendor != "any" {
			// Check vendor
			if gpu.Vendor == req.Vendor {
				matchingReqs = append(matchingReqs, req)
			}
		}
	}

	if len(matchingReqs) == 0 {
		// No requirements found for this GPU - not necessarily an error
		return result
	}

	hasErrors := false
	hasWarnings := false

	// Check against all matching requirements
	for _, req := range matchingReqs {
		// Check PCI lanes
		if req.MinPCILines > 0 {
			if gpu.PCILines == 0 {
				result.Issues = append(result.Issues, "Could not determine active PCI lanes")
				result.PCILinesWarn = true
				hasWarnings = true
			} else if gpu.PCILines < req.MinPCILines {
				result.Issues = append(result.Issues, fmt.Sprintf("PCI lanes: %d active (required %d)", gpu.PCILines, req.MinPCILines))
				result.PCILinesOK = false
				hasErrors = true
			}
		}

		// Check memory
		if req.MinMemoryMB > 0 {
			if gpu.MemoryMB == 0 {
				result.Issues = append(result.Issues, "Could not determine memory size")
				result.MemoryWarn = true
				hasWarnings = true
			} else if gpu.MemoryMB < req.MinMemoryMB {
				result.Issues = append(result.Issues, fmt.Sprintf("Memory: %d MB (required %d MB)", gpu.MemoryMB, req.MinMemoryMB))
				result.MemoryOK = false
				hasErrors = true
			}
		}

		// Check driver
		if req.Driver != "" {
			if gpu.Driver == "unknown" {
				result.Issues = append(result.Issues, "Could not determine driver")
				result.DriverWarn = true
				hasWarnings = true
			} else if gpu.Driver != req.Driver {
				result.Issues = append(result.Issues, fmt.Sprintf("Driver: %s (required %s)", gpu.Driver, req.Driver))
				result.DriverOK = false
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

// Обновленная функция visualizeSlots с проверкой требований
func visualizeSlots(gpus []GPUInfo, config *Config) error {
	printInfo("PCIe Slots Layout:")
	fmt.Println()

	// Determine number of slots to show
	maxSlots := config.Visualization.TotalSlots
	if maxSlots == 0 {
		maxSlots = len(gpus) + 2 // Fallback if not configured
	}

	// Create slot data array indexed by logical slot number
	slotData := make([]GPUInfo, maxSlots+1) // +1 because slots start from 1
	physicalSlots := make([]string, maxSlots+1)
	slotResults := make([]GPUCheckResult, maxSlots+1) // Check results for each slot

	// Fill slots based on PCI to slot mapping
	foundPCIs := make(map[string]bool)
	for _, gpu := range gpus {
		foundPCIs[gpu.PCISlot] = true
		if logicalSlot, exists := config.Visualization.PCIToSlot[gpu.PCISlot]; exists {
			if logicalSlot > 0 && logicalSlot <= maxSlots {
				slotData[logicalSlot] = gpu
				physicalSlots[logicalSlot] = gpu.PhysicalSlot

				// Check this GPU against requirements
				slotResults[logicalSlot] = checkGPUAgainstRequirements(gpu, config.GPURequirements)
			}
		}
	}

	// Check for missing devices
	hasErrors := false
	hasWarnings := false
	missingDevices := []string{}

	for pciAddr, expectedSlot := range config.Visualization.PCIToSlot {
		if expectedSlot > 0 && expectedSlot <= maxSlots {
			if !foundPCIs[pciAddr] {
				// Device should be here but is missing
				slotResults[expectedSlot] = GPUCheckResult{Status: "missing"}
				missingDevices = append(missingDevices, fmt.Sprintf("%s (slot %d)", pciAddr, expectedSlot))
				hasErrors = true

				// Create a placeholder for missing device
				slotData[expectedSlot] = GPUInfo{
					Name:    fmt.Sprintf("MISSING: %s", pciAddr),
					PCISlot: pciAddr,
				}
			}
		}
	}

	// Check for unexpected devices (found but not in mapping)
	unexpectedDevices := []string{}
	for _, gpu := range gpus {
		if _, exists := config.Visualization.PCIToSlot[gpu.PCISlot]; !exists {
			unexpectedDevices = append(unexpectedDevices, gpu.PCISlot)
			hasWarnings = true
		}
	}

	// Count status types for summary
	for i := 1; i <= maxSlots; i++ {
		status := slotResults[i].Status
		if status == "error" || status == "missing" {
			hasErrors = true
		} else if status == "warning" {
			hasWarnings = true
		}
	}

	// Print legend BEFORE the table
	printInfo("Legend:")
	fmt.Printf("  %s%s%s Present & OK  ", ColorGreen, "▓▓▓", ColorReset)
	fmt.Printf("  %s%s%s Device with Issues  ", ColorYellow, "▓▓▓", ColorReset)
	fmt.Printf("  %s%s%s Missing Device  ", ColorRed, "░░░", ColorReset)
	fmt.Printf("  %s%s%s Empty Slot\n", ColorWhite, "░░░", ColorReset)
	fmt.Println()

	// Report missing devices BEFORE the table
	if len(missingDevices) > 0 {
		printError("Missing devices:")
		for _, device := range missingDevices {
			printError(fmt.Sprintf("  - %s", device))
		}
		fmt.Println()
	}

	if len(unexpectedDevices) > 0 {
		printWarning("Unexpected devices (not in configuration mapping):")
		for _, pci := range unexpectedDevices {
			printWarning(fmt.Sprintf("  - %s", pci))
		}
		fmt.Println()
	}

	// Report detailed issues for each slot
	for i := 1; i <= maxSlots; i++ {
		result := slotResults[i]
		if len(result.Issues) > 0 {
			gpu := slotData[i]

			if result.Status == "error" {
				printError(fmt.Sprintf("Slot %d (%s) issues:", i, gpu.PCISlot))
			} else if result.Status == "warning" {
				printWarning(fmt.Sprintf("Slot %d (%s) warnings:", i, gpu.PCISlot))
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

	// Build visualization rows
	width := config.Visualization.SlotWidth

	// Top border
	fmt.Print("┌")
	for i := 1; i <= maxSlots; i++ {
		fmt.Print(strings.Repeat("─", width))
		if i < maxSlots {
			fmt.Print("┬")
		}
	}
	fmt.Println("┐")

	// Symbols row with color coding based on status
	fmt.Print("│")
	for i := 1; i <= maxSlots; i++ {
		visual := getDeviceVisual(slotData[i], &config.Visualization)
		result := slotResults[i]

		symbolText := centerText(visual.Symbol, width)

		switch result.Status {
		case "ok":
			fmt.Print(ColorGreen + symbolText + ColorReset)
		case "warning", "error":
			fmt.Print(ColorYellow + symbolText + ColorReset) // Слот желтый при проблемах
		case "missing":
			fmt.Print(ColorRed + centerText("░░░", width) + ColorReset)
		default:
			fmt.Print(symbolText) // Empty slot
		}
		fmt.Print("│")
	}
	fmt.Println()

	// Short names row with color coding
	fmt.Print("│")
	for i := 1; i <= maxSlots; i++ {
		result := slotResults[i]

		if slotData[i].Name != "" {
			visual := getDeviceVisual(slotData[i], &config.Visualization)
			nameText := centerText(visual.ShortName, width)

			switch result.Status {
			case "ok":
				fmt.Print(ColorGreen + nameText + ColorReset)
			case "warning", "error":
				fmt.Print(ColorYellow + nameText + ColorReset) // Слот желтый при проблемах
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

	// PCI lanes row with specific color coding for values
	fmt.Print("│")
	for i := 1; i <= maxSlots; i++ {
		result := slotResults[i]

		if slotData[i].Name != "" {
			var pciInfo string
			if result.Status == "missing" {
				pciInfo = "?"
			} else {
				if slotData[i].PCILines > 0 {
					pciInfo = fmt.Sprintf("x%d", slotData[i].PCILines)
				} else {
					pciInfo = "x?"
				}
			}

			pciText := centerText(pciInfo, width)

			if result.Status == "missing" {
				fmt.Print(ColorRed + pciText + ColorReset)
			} else if !result.PCILinesOK {
				fmt.Print(ColorRed + pciText + ColorReset) // Красным если не соответствует требованиям
			} else if result.Status == "warning" || result.Status == "error" {
				fmt.Print(ColorYellow + pciText + ColorReset) // Желтым если у слота есть проблемы
			} else if result.PCILinesWarn {
				fmt.Print(ColorYellow + pciText + ColorReset) // Желтым если предупреждение
			} else if result.Status == "ok" {
				fmt.Print(ColorGreen + pciText + ColorReset)
			} else {
				fmt.Print(pciText)
			}
		} else {
			fmt.Print(strings.Repeat(" ", width))
		}
		fmt.Print("│")
	}
	fmt.Println()

	// Memory row with specific color coding for values
	fmt.Print("│")
	for i := 1; i <= maxSlots; i++ {
		result := slotResults[i]

		if slotData[i].Name != "" {
			var memInfo string
			if result.Status == "missing" {
				memInfo = "?"
			} else {
				memInfo = formatMemory(slotData[i].MemoryMB)
			}

			memText := centerText(memInfo, width)

			if result.Status == "missing" {
				fmt.Print(ColorRed + memText + ColorReset)
			} else if !result.MemoryOK {
				fmt.Print(ColorRed + memText + ColorReset) // Красным если не соответствует требованиям
			} else if result.Status == "warning" || result.Status == "error" {
				fmt.Print(ColorYellow + memText + ColorReset) // Желтым если у слота есть проблемы
			} else if result.MemoryWarn {
				fmt.Print(ColorYellow + memText + ColorReset) // Желтым если предупреждение
			} else if result.Status == "ok" {
				fmt.Print(ColorGreen + memText + ColorReset)
			} else {
				fmt.Print(memText)
			}
		} else {
			fmt.Print(strings.Repeat(" ", width))
		}
		fmt.Print("│")
	}
	fmt.Println()

	// Bottom border
	fmt.Print("└")
	for i := 1; i <= maxSlots; i++ {
		fmt.Print(strings.Repeat("─", width))
		if i < maxSlots {
			fmt.Print("┴")
		}
	}
	fmt.Println("┘")

	// Logical slot numbers
	fmt.Print(" ")
	for i := 1; i <= maxSlots; i++ {
		fmt.Print(centerText(fmt.Sprintf("%d", i), width+1))
	}
	fmt.Println("(Logic)")

	// Physical slot numbers
	fmt.Print(" ")
	for i := 1; i <= maxSlots; i++ {
		physSlot := physicalSlots[i]
		if physSlot == "" || physSlot == "unknown" {
			physSlot = "-"
		}
		fmt.Print(centerText(physSlot, width+1))
	}
	fmt.Println("(Physical)")

	// PCI addresses row
	fmt.Print(" ")
	for i := 1; i <= maxSlots; i++ {
		pciAddr := ""
		if slotData[i].Name != "" {
			pciAddr = slotData[i].PCISlot
		} else {
			pciAddr = "-"
		}
		fmt.Print(centerText(pciAddr, width+1))
	}
	fmt.Println("(PCI)")

	fmt.Println()

	// Final status message
	if hasErrors {
		printError("Configuration validation FAILED!")
		return fmt.Errorf("GPU configuration validation failed")
	} else if hasWarnings {
		printWarning("Configuration validation completed with warnings")
		return nil
	} else {
		printSuccess("All devices present and meet requirements!")
		return nil
	}
}

func getDeviceVisual(gpu GPUInfo, config *VisualizationConfig) DeviceVisual {
	// If GPU is empty (name is empty), return empty slot
	if gpu.Name == "" {
		return DeviceVisual{
			Symbol:      "░░░",
			ShortName:   "",
			Description: "Empty Slot",
			Color:       "gray",
		}
	}

	// Check if we have specific mapping for this device ID
	if gpu.DeviceID != "" {
		if visual, exists := config.DeviceMap[gpu.DeviceID]; exists {
			return visual
		}
	}

	// Generate fallback visual for unknown devices
	return DeviceVisual{
		Symbol:      generateSymbol(gpu),
		ShortName:   generateShortName(gpu),
		Description: gpu.Name,
		Color:       generateColor(gpu.Name),
	}
}

func checkGPU(config *Config) error {
	printInfo("Starting GPU check...")

	gpus, err := getGPUInfo()
	if err != nil {
		return fmt.Errorf("failed to get GPU info: %v", err)
	}

	printInfo(fmt.Sprintf("Found GPU devices: %d", len(gpus)))

	if len(gpus) == 0 {
		printError("No GPU devices found")
		return fmt.Errorf("no GPU devices found")
	}

	// Display found GPUs
	for i, gpu := range gpus {
		printInfo(fmt.Sprintf("GPU %d: %s", i+1, gpu.Name))
		printDebug(fmt.Sprintf("  PCI Slot: %s", gpu.PCISlot))
		printDebug(fmt.Sprintf("  Physical Slot: %s", gpu.PhysicalSlot))
		printDebug(fmt.Sprintf("  Vendor: %s", gpu.Vendor))
		printDebug(fmt.Sprintf("  Device ID: %s", gpu.DeviceID))
		printDebug(fmt.Sprintf("  Driver: %s", gpu.Driver))
		printDebug(fmt.Sprintf("  PCI Lines (active): %d", gpu.PCILines))
		printDebug(fmt.Sprintf("  Memory: %d MB", gpu.MemoryMB))
	}

	// Check each requirement
	allPassed := true
	for _, req := range config.GPURequirements {
		printInfo(fmt.Sprintf("Checking requirement: %s", req.Name))

		matchingGPUs := filterGPUs(gpus, req)

		printInfo(fmt.Sprintf("  Found %d GPU(s) matching criteria", len(matchingGPUs)))

		if len(matchingGPUs) < req.MinCount {
			printError(fmt.Sprintf("  Requirement FAILED: found %d GPU(s), required %d", len(matchingGPUs), req.MinCount))
			allPassed = false
			continue
		}

		if req.MinCount == 0 {
			printInfo(fmt.Sprintf("  Requirement OK: optional check passed"))
			continue
		}

		// Check each matching GPU against the requirements
		reqPassed := true
		for i, gpu := range matchingGPUs {
			printInfo(fmt.Sprintf("    GPU %d: %s", i+1, gpu.Name))

			// Check PCI lanes
			if req.MinPCILines > 0 {
				if gpu.PCILines == 0 {
					printWarning(fmt.Sprintf("      Could not determine active PCI lanes"))
				} else if gpu.PCILines < req.MinPCILines {
					printError(fmt.Sprintf("      PCI lanes FAILED: %d active (required %d)", gpu.PCILines, req.MinPCILines))
					reqPassed = false
				} else {
					printSuccess(fmt.Sprintf("      PCI lanes OK: %d active", gpu.PCILines))
				}
			}

			// Check memory
			if req.MinMemoryMB > 0 {
				if gpu.MemoryMB == 0 {
					printWarning(fmt.Sprintf("      Could not determine memory size"))
				} else if gpu.MemoryMB < req.MinMemoryMB {
					printError(fmt.Sprintf("      Memory FAILED: %d MB (required %d MB)", gpu.MemoryMB, req.MinMemoryMB))
					reqPassed = false
				} else {
					printSuccess(fmt.Sprintf("      Memory OK: %d MB", gpu.MemoryMB))
				}
			}

			// Check driver
			if req.Driver != "" {
				if gpu.Driver == "unknown" {
					printWarning(fmt.Sprintf("      Could not determine driver"))
				} else if gpu.Driver != req.Driver {
					printError(fmt.Sprintf("      Driver FAILED: %s (required %s)", gpu.Driver, req.Driver))
					reqPassed = false
				} else {
					printSuccess(fmt.Sprintf("      Driver OK: %s", gpu.Driver))
				}
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
		printSuccess("All GPU requirements passed")
	} else {
		printError("Some GPU requirements failed")
		return fmt.Errorf("GPU requirements not met")
	}

	return nil
}

func filterGPUs(gpus []GPUInfo, req GPURequirement) []GPUInfo {
	var matching []GPUInfo

	for _, gpu := range gpus {
		// Check specific device IDs first (most precise)
		if len(req.DeviceIDs) > 0 {
			found := false
			for _, deviceID := range req.DeviceIDs {
				if gpu.DeviceID == deviceID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		} else {
			// Check vendor filter if no specific device IDs
			if req.Vendor != "" && req.Vendor != "any" && gpu.Vendor != req.Vendor {
				continue
			}
		}

		matching = append(matching, gpu)
	}

	return matching
}

func main() {
	var (
		showVersion  = flag.Bool("V", false, "Show version")
		configPath   = flag.String("c", "gpu_config.json", "Path to configuration file")
		createConfig = flag.Bool("s", false, "Create default configuration file")
		showHelpFlag = flag.Bool("h", false, "Show help")
		listOnly     = flag.Bool("l", false, "List detected GPUs without configuration check")
		visualize    = flag.Bool("vis", false, "Show visual PCIe slots layout")
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
		printInfo("Scanning for GPU devices...")
		gpus, err := getGPUInfo()
		if err != nil {
			printError(fmt.Sprintf("Error getting GPU information: %v", err))
			os.Exit(1)
		}

		if len(gpus) == 0 {
			printWarning("No GPU devices found")
		} else {
			printSuccess(fmt.Sprintf("Found GPU devices: %d", len(gpus)))
			for i, gpu := range gpus {
				fmt.Printf("\nGPU %d:\n", i+1)
				fmt.Printf("  Name: %s\n", gpu.Name)
				fmt.Printf("  PCI Slot: %s\n", gpu.PCISlot)
				fmt.Printf("  Physical Slot: %s\n", gpu.PhysicalSlot)
				fmt.Printf("  Vendor: %s\n", gpu.Vendor)
				fmt.Printf("  Device ID: %s\n", gpu.DeviceID)
				fmt.Printf("  Driver: %s\n", gpu.Driver)
				fmt.Printf("  PCI Lines (active): %d\n", gpu.PCILines)
				fmt.Printf("  Memory: %d MB\n", gpu.MemoryMB)
			}
		}
		return
	}

	if *visualize {
		printInfo("Scanning for GPU devices...")
		gpus, err := getGPUInfo()
		if err != nil {
			printError(fmt.Sprintf("Error getting GPU information: %v", err))
			os.Exit(1)
		}

		// Load configuration for requirements checking
		config, err := loadConfig(*configPath)
		if err != nil {
			printError(fmt.Sprintf("Error loading configuration: %v", err))
			printInfo("Use -s to create a default configuration file")
			printInfo("Cannot perform requirements checking without configuration")
			os.Exit(1)
		}

		err = visualizeSlots(gpus, config)
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

	// Load configuration
	config, err := loadConfig(*configPath)
	if err != nil {
		printError(fmt.Sprintf("Error loading configuration: %v", err))
		printInfo("Use -s to create a default configuration file")
		printInfo("Or use -l to simply display found GPUs")
		os.Exit(1)
	}

	printInfo(fmt.Sprintf("Configuration loaded from: %s", *configPath))

	// Perform GPU check
	err = checkGPU(config)
	if err != nil {
		printError(fmt.Sprintf("GPU check failed: %v", err))
		os.Exit(1)
	}
}
