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

const VERSION = "1.0.0"

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
	Name        string `json:"name"`
	MinPCILines int    `json:"min_pci_lines"`
	MinMemoryMB int    `json:"min_memory_mb"`
	Driver      string `json:"driver"`
	Vendor      string `json:"vendor"`
	MinCount    int    `json:"min_count"`
}

type Config struct {
	GPURequirements []GPURequirement `json:"gpu_requirements"`
	CheckPower      bool             `json:"check_power"`
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
	fmt.Println("  -d          Show detailed debug information")
	fmt.Println("  -h          Show this help")
}

func createDefaultConfig(configPath string) error {
	printInfo("Scanning system for GPU devices to create configuration...")

	// Get current GPU information
	gpus, err := getGPUInfo()
	if err != nil {
		printWarning(fmt.Sprintf("Could not scan GPUs: %v", err))
		printInfo("Creating minimal default configuration...")
		return createMinimalConfig(configPath)
	}

	if len(gpus) == 0 {
		printWarning("No GPU devices found in system")
		printInfo("Creating minimal default configuration...")
		return createMinimalConfig(configPath)
	}

	printInfo(fmt.Sprintf("Found %d GPU device(s), creating configuration based on detected hardware:", len(gpus)))

	var requirements []GPURequirement

	// Display found GPUs and create requirements
	for i, gpu := range gpus {
		printInfo(fmt.Sprintf("  GPU %d: %s (%s)", i+1, gpu.Name, gpu.Vendor))
		printInfo(fmt.Sprintf("    PCI Slot: %s, Physical Slot: %s", gpu.PCISlot, gpu.PhysicalSlot))
		printInfo(fmt.Sprintf("    PCI Lines: %d, Memory: %d MB, Driver: %s", gpu.PCILines, gpu.MemoryMB, gpu.Driver))

		// Create requirement based on this GPU
		req := GPURequirement{
			Name:     fmt.Sprintf("%s GPU", capitalizeVendor(gpu.Vendor)),
			Vendor:   gpu.Vendor,
			MinCount: 1,
		}

		// Set PCI lanes requirement (with some tolerance)
		if gpu.PCILines > 0 {
			req.MinPCILines = gpu.PCILines
		} else {
			// Default reasonable values based on vendor
			switch gpu.Vendor {
			case "nvidia":
				req.MinPCILines = 16
			case "amd":
				req.MinPCILines = 16
			case "intel":
				req.MinPCILines = 4
			default:
				req.MinPCILines = 8
			}
		}

		// Set memory requirement (use detected or reasonable default)
		if gpu.MemoryMB > 0 {
			// Use detected memory as minimum requirement
			req.MinMemoryMB = gpu.MemoryMB
		} else {
			// Default reasonable values based on vendor
			switch gpu.Vendor {
			case "nvidia":
				req.MinMemoryMB = 4096
			case "amd":
				req.MinMemoryMB = 4096
			case "intel":
				req.MinMemoryMB = 1024
			default:
				req.MinMemoryMB = 2048
			}
		}

		// Set driver requirement if known
		if gpu.Driver != "unknown" && gpu.Driver != "" {
			req.Driver = gpu.Driver
		}

		// Check if we already have a requirement for this vendor
		existingIndex := -1
		for j, existing := range requirements {
			if existing.Vendor == gpu.Vendor {
				existingIndex = j
				break
			}
		}

		if existingIndex >= 0 {
			// Update existing requirement
			existing := &requirements[existingIndex]
			existing.MinCount++

			// Use the higher memory requirement
			if req.MinMemoryMB > existing.MinMemoryMB {
				existing.MinMemoryMB = req.MinMemoryMB
			}

			// Use the higher PCI lanes requirement
			if req.MinPCILines > existing.MinPCILines {
				existing.MinPCILines = req.MinPCILines
			}

			// Update name to reflect multiple GPUs
			existing.Name = fmt.Sprintf("%s GPUs (minimum %d)", capitalizeVendor(gpu.Vendor), existing.MinCount)
		} else {
			// Add new requirement
			requirements = append(requirements, req)
		}
	}

	// Add a general fallback requirement
	requirements = append(requirements, GPURequirement{
		Name:        "Any additional GPU (optional)",
		MinPCILines: 4,
		MinMemoryMB: 1024,
		Driver:      "",
		Vendor:      "any",
		MinCount:    0,
	})

	config := Config{
		GPURequirements: requirements,
		CheckPower:      true,
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
	printInfo("You can edit the configuration file to adjust requirements as needed")

	return nil
}

func createMinimalConfig(configPath string) error {
	defaultConfig := Config{
		GPURequirements: []GPURequirement{
			{
				Name:        "Any GPU",
				MinPCILines: 8,
				MinMemoryMB: 2048,
				Driver:      "",
				Vendor:      "any",
				MinCount:    1,
			},
		},
		CheckPower: true,
	}

	data, err := json.MarshalIndent(defaultConfig, "", "  ")
	if err != nil {
		return err
	}

	err = os.MkdirAll(filepath.Dir(configPath), 0755)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(configPath, data, 0644)
}

func capitalizeVendor(vendor string) string {
	switch strings.ToLower(vendor) {
	case "nvidia":
		return "NVIDIA"
	case "amd":
		return "AMD"
	case "intel":
		return "Intel"
	default:
		return strings.Title(vendor)
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

		// Parse PCI capabilities for ACTIVE lane information
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

	// Determine vendor
	if strings.Contains(nameLower, "nvidia") {
		vendor = "nvidia"
	} else if strings.Contains(nameLower, "amd") || strings.Contains(nameLower, "ati") {
		vendor = "amd"
	} else if strings.Contains(nameLower, "intel") {
		vendor = "intel"
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
		// Check vendor filter
		if req.Vendor != "" && req.Vendor != "any" && gpu.Vendor != req.Vendor {
			continue
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
