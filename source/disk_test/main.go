package main

import (
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

type DiskInfo struct {
	Slot        string `json:"slot"`        // Physical slot (HCTL: "1:0:0:0" or PCI: "0000:06:00.0" or USB: "1-2:1.0")
	SlotType    string `json:"slot_type"`   // "SATA", "NVMe", "USB"
	Device      string `json:"device"`      // /dev/sda, /dev/nvme0n1, etc.
	IsPresent   bool   `json:"is_present"`  // Whether device is present in slot
	DiskType    string `json:"disk_type"`   // "SSD", "HDD", "USB", "Unknown"
	Model       string `json:"model"`       // Device model
	SizeGB      int    `json:"size_gb"`     // Size in GB
	Temperature int    `json:"temperature"` // Temperature via SMART (0 if unavailable)
	Serial      string `json:"serial"`      // Serial number
	Health      string `json:"health"`      // SMART status ("OK", "FAILING", "N/A")

	// USB specific fields
	USBBus     string `json:"usb_bus,omitempty"`     // USB bus number (e.g., "001")
	USBDevice  string `json:"usb_device,omitempty"`  // USB device number (e.g., "002")
	USBVersion string `json:"usb_version,omitempty"` // USB version (e.g., "2.0", "3.0", "3.1")
	USBPort    string `json:"usb_port,omitempty"`    // USB port identifier (e.g., "1-2")
}

type DiskRequirement struct {
	Name          string   `json:"name"`
	SlotType      string   `json:"slot_type"`             // "SATA", "NVMe", "USB", "any"
	RequiredSlots []string `json:"required_slots"`        // Specific slots that must be populated
	MinDisks      int      `json:"min_disks"`             // Minimum number of disks
	MaxDisks      int      `json:"max_disks"`             // Maximum number of disks
	MinSizeGB     int      `json:"min_size_gb"`           // Minimum disk size
	MaxTempC      int      `json:"max_temp_c"`            // Maximum temperature
	RequiredType  string   `json:"required_type"`         // Required disk type (SSD, HDD, etc.)
	CheckSMART    bool     `json:"check_smart"`           // Whether to check SMART health
	USBVersion    string   `json:"usb_version,omitempty"` // Required USB version for USB devices
}

type DiskVisual struct {
	Symbol      string `json:"symbol"`
	ShortName   string `json:"short_name"`
	Description string `json:"description"`
	Color       string `json:"color"`
}

type RowConfig struct {
	Name  string `json:"name"`  // Display name for the row
	Slots string `json:"slots"` // Slot range (e.g., "1-4", "5-8")
}

type CustomRowsConfig struct {
	Enabled bool        `json:"enabled"` // Enable custom row configuration
	Rows    []RowConfig `json:"rows"`    // Custom row definitions
}

type VisualizationConfig struct {
	TypeVisuals map[string]DiskVisual `json:"type_visuals"`  // disk type -> visual
	SlotMapping map[string]int        `json:"slot_mapping"`  // slot address -> logical position
	TotalSlots  int                   `json:"total_slots"`   // Total disk slots
	SlotWidth   int                   `json:"slot_width"`    // Width of each slot in visualization
	SlotsPerRow int                   `json:"slots_per_row"` // Number of slots per row (legacy)
	CustomRows  CustomRowsConfig      `json:"custom_rows"`   // Custom row configuration
}

type Config struct {
	DiskRequirements []DiskRequirement   `json:"disk_requirements"`
	Visualization    VisualizationConfig `json:"visualization"`
	CheckTemperature bool                `json:"check_temperature"`
	CheckSMART       bool                `json:"check_smart"`
	SMARTTimeout     int                 `json:"smart_timeout_seconds"`
}

type DiskCheckResult struct {
	Status     string // "ok", "warning", "error", "missing"
	Issues     []string
	SizeOK     bool
	TempOK     bool
	TypeOK     bool
	HealthOK   bool
	SizeWarn   bool
	TempWarn   bool
	TypeWarn   bool
	HealthWarn bool
}

// ANSI color codes
const (
	ColorReset  = "\033[0m"
	ColorGreen  = "\033[92m"
	ColorBlue   = "\033[34m"
	ColorWhite  = "\033[37m"
	ColorYellow = "\033[33m"
	ColorRed    = "\033[31m"
	ColorGray   = "\033[90m"
	ColorCyan   = "\033[36m"
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

func getANSIColor(name string) string {
	switch strings.ToLower(name) {
	case "green":
		return ColorGreen
	case "blue":
		return ColorBlue
	case "yellow":
		return ColorYellow
	case "red":
		return ColorRed
	case "gray", "grey":
		return ColorGray
	case "white":
		return ColorWhite
	case "cyan":
		return ColorCyan
	default:
		return ColorWhite
	}
}

func showHelp() {
	fmt.Printf("Disk Checker %s\n", VERSION)
	fmt.Println("Parameters:")
	fmt.Println("  -V          Show program version")
	fmt.Println("  -c <path>   Path to configuration file")
	fmt.Println("  -s          Create default configuration file")
	fmt.Println("  -l          List detected disks without configuration check")
	fmt.Println("  -vis        Show visual disk slots layout")
	fmt.Println("  -d          Show detailed debug information")
	fmt.Println("  -h          Show this help")
}

func getDiskInfo() ([]DiskInfo, error) {
	var disks []DiskInfo

	sataDisks, err := getSATADisks()
	if err != nil {
		printDebug(fmt.Sprintf("SATA disk detection failed: %v", err))
	}
	disks = append(disks, sataDisks...)

	nvmeDisks, err := getNVMeDisks()
	if err != nil {
		printDebug(fmt.Sprintf("NVMe disk detection failed: %v", err))
	}
	disks = append(disks, nvmeDisks...)

	usbDisks, err := getUSBDisks()
	if err != nil {
		printDebug(fmt.Sprintf("USB disk detection failed: %v", err))
	}
	disks = append(disks, usbDisks...)

	for i := range disks {
		if disks[i].IsPresent {
			enrichDiskWithSMART(&disks[i])
		}
	}

	if len(disks) == 0 {
		return nil, fmt.Errorf("no disk slots found")
	}

	printDebug(fmt.Sprintf("Total detected disk slots: %d", len(disks)))
	return disks, nil
}

func getSATADisks() ([]DiskInfo, error) {
	var disks []DiskInfo

	// Find SATA devices through SCSI subsystem, but only main devices (not partitions)
	paths, err := filepath.Glob("/sys/class/scsi_device/*/device/block/sd[a-z]")
	if err != nil {
		return nil, err
	}

	for _, blockPath := range paths {
		devName := filepath.Base(blockPath)             // sda, sdb, etc.
		parent := filepath.Dir(filepath.Dir(blockPath)) // /sys/class/scsi_device/H:C:T:L/device

		hctl := filepath.Base(filepath.Dir(parent)) // H:C:T:L
		if !regexp.MustCompile(`^\d+:\d+:\d+:\d+$`).MatchString(hctl) {
			continue
		}

		device := "/dev/" + devName

		// Skip USB devices - they are handled separately
		if isUSBDevice(devName) {
			printDebug(fmt.Sprintf("Skipping USB device %s in SATA scan", devName))
			continue
		}

		// Read vendor/model
		vendor := readSysFile(filepath.Join(parent, "vendor"))
		model := readSysFile(filepath.Join(parent, "model"))
		serial := readSysFile(filepath.Join(parent, "rev")) // fallback, not real serial

		disk := DiskInfo{
			Slot:      hctl,
			SlotType:  "SATA",
			IsPresent: true,
			Device:    device,
			Model:     strings.TrimSpace(vendor + " " + model),
			Serial:    strings.TrimSpace(serial),
			SizeGB:    getBlockDeviceSize(devName),
			DiskType:  determineDiskType(device, "SATA"),
		}

		disks = append(disks, disk)
		printDebug(fmt.Sprintf("Detected SATA slot %s (%s)", hctl, devName))
	}

	return disks, nil
}

func isUSBDevice(devName string) bool {
	// Check multiple ways to determine if device is USB

	// Method 1: Check symlink path
	realDevPath, err := filepath.EvalSymlinks(filepath.Join("/sys/block", devName))
	if err == nil && strings.Contains(realDevPath, "/usb") {
		return true
	}

	// Method 2: Check if device has USB subsystem
	subsystemPath := filepath.Join("/sys/block", devName, "device/subsystem")
	if subsystem, err := filepath.EvalSymlinks(subsystemPath); err == nil {
		if strings.Contains(subsystem, "usb") {
			return true
		}
	}

	// Method 3: Check if device path contains USB controllers
	devicePath := filepath.Join("/sys/block", devName, "device")
	if realPath, err := filepath.EvalSymlinks(devicePath); err == nil {
		// Look for USB identifiers in the path
		if strings.Contains(realPath, "/usb") ||
			regexp.MustCompile(`/\d+-\d+[:\.]`).MatchString(realPath) {
			return true
		}
	}

	// Method 4: Check modalias for USB
	modaliasPath := filepath.Join("/sys/block", devName, "device/modalias")
	if modalias := readSysFile(modaliasPath); modalias != "" {
		if strings.HasPrefix(modalias, "usb:") {
			return true
		}
	}

	return false
}

func getUSBDisks() ([]DiskInfo, error) {
	var disks []DiskInfo

	// Find USB storage devices through /sys/class/block, but only main devices (not partitions)
	blockPaths, err := filepath.Glob("/sys/class/block/sd[a-z]")
	if err != nil {
		return nil, err
	}

	for _, blockPath := range blockPaths {
		devName := filepath.Base(blockPath) // sda, sdb, etc.
		device := "/dev/" + devName

		// Only process USB devices
		if !isUSBDevice(devName) {
			continue
		}

		realDevPath, err := filepath.EvalSymlinks(blockPath)
		if err != nil {
			printDebug(fmt.Sprintf("Failed to get real path for %s: %v", devName, err))
			continue
		}

		usbInfo, err := parseUSBDeviceInfo(realDevPath)
		if err != nil {
			printDebug(fmt.Sprintf("Failed to parse USB info for %s: %v", devName, err))
			continue
		}

		// Get device vendor/model from USB attributes
		vendor := usbInfo.Vendor
		model := usbInfo.Product
		if vendor == "" || model == "" {
			// Fallback to SCSI attributes if USB attributes are missing
			if scsiPath := findSCSIPath(realDevPath); scsiPath != "" {
				vendor = readSysFile(filepath.Join(scsiPath, "vendor"))
				model = readSysFile(filepath.Join(scsiPath, "model"))
			}
		}

		// Create unique slot identifier: "USB:bus-port:version"
		slotID := fmt.Sprintf("USB:%s-%s:%s", usbInfo.Bus, usbInfo.Port, usbInfo.Version)

		disk := DiskInfo{
			Slot:       slotID,
			SlotType:   "USB",
			IsPresent:  true,
			Device:     device,
			Model:      strings.TrimSpace(vendor + " " + model),
			Serial:     usbInfo.Serial,
			SizeGB:     getBlockDeviceSize(devName),
			DiskType:   "USB",
			USBBus:     usbInfo.Bus,
			USBDevice:  usbInfo.Device,
			USBVersion: usbInfo.Version,
			USBPort:    usbInfo.Port,
		}

		disks = append(disks, disk)
		printDebug(fmt.Sprintf("Detected USB slot %s (%s) [%s %s USB %s]", slotID, devName, vendor, model, usbInfo.Version))
	}

	return disks, nil
}

type USBDeviceInfo struct {
	Bus     string
	Device  string
	Port    string
	Version string
	Vendor  string
	Product string
	Serial  string
}

func parseUSBDeviceInfo(devicePath string) (*USBDeviceInfo, error) {
	// devicePath looks like: /sys/devices/pci0000:00/0000:00:14.0/usb1/1-2/1-2:1.0/host4/target4:0:0/4:0:0:0/block/sda

	// Find USB device directory (contains bus and port info)
	pathParts := strings.Split(devicePath, "/")
	var usbDevPath string
	var portPattern string

	for i, part := range pathParts {
		// Look for pattern like "1-2" (bus-port)
		if regexp.MustCompile(`^\d+-[\d.]+$`).MatchString(part) {
			portPattern = part
			usbDevPath = strings.Join(pathParts[:i+1], "/")
			break
		}
	}

	if usbDevPath == "" || portPattern == "" {
		return nil, fmt.Errorf("USB device path not found in %s", devicePath)
	}

	// Extract bus number from port pattern (e.g., "1-2" -> bus "1")
	busNum := strings.Split(portPattern, "-")[0]

	// Read USB device information
	info := &USBDeviceInfo{
		Bus:  fmt.Sprintf("%03s", busNum), // Format as 3-digit number
		Port: portPattern,
	}

	// Read USB version
	if version := readSysFile(filepath.Join(usbDevPath, "version")); version != "" {
		info.Version = parseUSBVersion(version)
	} else if speed := readSysFile(filepath.Join(usbDevPath, "speed")); speed != "" {
		info.Version = speedToUSBVersion(speed)
	}

	// Read vendor and product info
	if vendor := readSysFile(filepath.Join(usbDevPath, "manufacturer")); vendor != "" {
		info.Vendor = vendor
	}
	if product := readSysFile(filepath.Join(usbDevPath, "product")); product != "" {
		info.Product = product
	}
	if serial := readSysFile(filepath.Join(usbDevPath, "serial")); serial != "" {
		info.Serial = serial
	}

	// Get device number from bus devices
	if devNum := findUSBDeviceNumber(busNum, usbDevPath); devNum != "" {
		info.Device = fmt.Sprintf("%03s", devNum)
	}

	return info, nil
}

func parseUSBVersion(versionStr string) string {
	// Version string format: " 2.00", " 3.00", etc.
	versionStr = strings.TrimSpace(versionStr)
	if versionStr == "" {
		return "Unknown"
	}

	switch {
	case strings.HasPrefix(versionStr, "1."):
		return "1.1"
	case strings.HasPrefix(versionStr, "2."):
		return "2.0"
	case strings.HasPrefix(versionStr, "3.0"):
		return "3.0"
	case strings.HasPrefix(versionStr, "3.1"):
		return "3.1"
	case strings.HasPrefix(versionStr, "3.2"):
		return "3.2"
	case strings.HasPrefix(versionStr, "3."):
		return "3.0"
	default:
		return versionStr
	}
}

func speedToUSBVersion(speedStr string) string {
	// Speed in Mbps: 1.5, 12, 480, 5000, 10000, etc.
	speedStr = strings.TrimSpace(speedStr)
	if speedStr == "" {
		return "Unknown"
	}

	if speed, err := strconv.ParseFloat(speedStr, 64); err == nil {
		switch {
		case speed <= 12:
			return "1.1"
		case speed <= 480:
			return "2.0"
		case speed <= 5000:
			return "3.0"
		case speed <= 10000:
			return "3.1"
		default:
			return "3.2"
		}
	}

	return "Unknown"
}

func findUSBDeviceNumber(busNum, usbDevPath string) string {
	// Try to find device number by looking at /dev/bus/usb/BUS/
	busDirPath := fmt.Sprintf("/dev/bus/usb/%s", busNum)
	if busDir, err := os.ReadDir(busDirPath); err == nil {
		for _, entry := range busDir {
			devPath := filepath.Join(busDirPath, entry.Name())
			if devRealPath, err := filepath.EvalSymlinks(devPath); err == nil {
				if strings.Contains(devRealPath, filepath.Base(usbDevPath)) {
					return entry.Name()
				}
			}
		}
	}

	// Fallback: read devnum file
	if devNum := readSysFile(filepath.Join(usbDevPath, "devnum")); devNum != "" {
		return devNum
	}

	return "000"
}

func findSCSIPath(devicePath string) string {
	// Find SCSI device path for getting vendor/model info
	pathParts := strings.Split(devicePath, "/")
	for i, part := range pathParts {
		if regexp.MustCompile(`^\d+:\d+:\d+:\d+$`).MatchString(part) {
			return strings.Join(pathParts[:i+1], "/")
		}
	}
	return ""
}

func getNVMeDisks() ([]DiskInfo, error) {
	var disks []DiskInfo

	// Ищем nvme контроллеры
	nvmePaths, err := filepath.Glob("/sys/class/nvme/nvme*")
	if err != nil {
		return nil, err
	}

	for _, nvmePath := range nvmePaths {
		nvmeName := filepath.Base(nvmePath) // nvme0, nvme1, ...

		// Получаем namespace, например nvme0n1
		nsPath := filepath.Join("/sys/block", nvmeName+"n1")
		if _, err := os.Stat(nsPath); os.IsNotExist(err) {
			continue // пропускаем, если namespace отсутствует
		}

		device := "/dev/" + nvmeName + "n1"

		// Получаем PCI-адрес
		deviceLink := filepath.Join(nvmePath, "device")
		realPath, err := filepath.EvalSymlinks(deviceLink)
		if err != nil {
			continue
		}
		pciRegex := regexp.MustCompile(`(\d{4}:\d{2}:\d{2}\.\d)`)
		matches := pciRegex.FindStringSubmatch(realPath)
		pciSlot := "unknown"
		if len(matches) >= 2 {
			pciSlot = matches[1]
		}

		model := strings.TrimSpace(readSysFile(filepath.Join(nvmePath, "model")))
		serial := strings.TrimSpace(readSysFile(filepath.Join(nvmePath, "serial")))

		disk := DiskInfo{
			Slot:      pciSlot,
			SlotType:  "NVMe",
			IsPresent: true,
			Device:    device,
			Model:     model,
			Serial:    serial,
			SizeGB:    getBlockDeviceSize(nvmeName + "n1"),
			DiskType:  "NVMe",
		}

		disks = append(disks, disk)
		printDebug(fmt.Sprintf("Detected NVMe slot %s (%s) [%s]", pciSlot, device, model))
	}

	return disks, nil
}

func readSysFile(path string) string {
	if data, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}

func getBlockDeviceSize(deviceName string) int {
	sizePath := fmt.Sprintf("/sys/block/%s/size", deviceName)
	sizeStr := readSysFile(sizePath)
	if sizeStr == "" {
		return 0
	}

	// Size is in 512-byte sectors
	if sectors, err := strconv.Atoi(sizeStr); err == nil {
		return (sectors * 512) / (1024 * 1024 * 1024) // Convert to GB
	}
	return 0
}

func determineDiskType(device, slotType string) string {
	deviceName := strings.TrimPrefix(device, "/dev/")

	switch slotType {
	case "USB":
		return "USB"
	case "NVMe":
		return "NVMe"
	case "SATA":
		rotationalPath := fmt.Sprintf("/sys/block/%s/queue/rotational", deviceName)
		rotational := readSysFile(rotationalPath)
		if rotational == "0" {
			return "SSD"
		} else if rotational == "1" {
			return "HDD"
		}
	}

	return "Unknown"
}

func getSMARTHealth(device string) string {
	cmd := exec.Command("smartctl", "-H", device)
	output, err := cmd.Output()
	if err != nil {
		return "N/A"
	}

	outputStr := string(output)
	if strings.Contains(outputStr, "PASSED") {
		return "OK"
	} else if strings.Contains(outputStr, "FAILED") {
		return "FAILING"
	}
	return "N/A"
}

func parseTemperatureFromLines(lines []string) int {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)

		// Method 1: Standard ATA temperature attribute (ID 194)
		if strings.Contains(lower, "temperature") && strings.Contains(line, "194") {
			if temp := parseATATemperatureLine(line); temp > 0 {
				return temp
			}
		}

		// Method 2: Other temperature attribute IDs (190, 231)
		if (strings.Contains(line, "190") || strings.Contains(line, "231")) && strings.Contains(lower, "temp") {
			if temp := parseATATemperatureLine(line); temp > 0 {
				return temp
			}
		}

		// Method 3: NVMe style temperature reporting
		if strings.Contains(line, "Temperature:") && strings.Contains(line, "Celsius") {
			re := regexp.MustCompile(`Temperature:\s+(\d+)\s+Celsius`)
			if matches := re.FindStringSubmatch(line); len(matches) == 2 {
				if val, err := strconv.Atoi(matches[1]); err == nil && val > 0 && val < 150 {
					return val
				}
			}
		}

		// Method 4: Alternative temperature formats
		// "Temperature_Celsius"
		if strings.Contains(lower, "temperature_celsius") {
			if temp := parseATATemperatureLine(line); temp > 0 {
				return temp
			}
		}

		// Method 5: Airflow temperature (some drives)
		if strings.Contains(lower, "airflow_temperature") {
			if temp := parseATATemperatureLine(line); temp > 0 {
				return temp
			}
		}

		// Method 6: Drive temperature (some Samsung drives)
		if strings.Contains(lower, "drive_temperature") {
			if temp := parseATATemperatureLine(line); temp > 0 {
				return temp
			}
		}

		// Method 7: Simple "Temperature" anywhere in line with number
		if strings.Contains(lower, "temperature") {
			// Look for temperature value in the line
			re := regexp.MustCompile(`(\d+)\s*°?c`)
			if matches := re.FindStringSubmatch(lower); len(matches) >= 2 {
				if val, err := strconv.Atoi(matches[1]); err == nil && val > 0 && val < 150 {
					return val
				}
			}
		}
	}

	return 0
}

func parseATATemperatureLine(line string) int {
	// Parse ATA SMART attribute line
	// Format: ID# ATTRIBUTE_NAME FLAG VALUE WORST THRESH TYPE UPDATED WHEN_FAILED RAW_VALUE
	fields := strings.Fields(line)

	if len(fields) >= 10 {
		// Try raw value (last field)
		raw := fields[len(fields)-1]

		// Raw value might be "45 (Min/Max 20/46)" - extract first number
		if temp := extractFirstNumber(raw); temp > 0 && temp < 150 {
			return temp
		}
	}

	if len(fields) >= 6 {
		// Try VALUE field (5th field, index 4)
		if val, err := strconv.Atoi(fields[4]); err == nil && val > 0 && val < 150 {
			return val
		}
	}

	return 0
}

func extractFirstNumber(str string) int {
	// Extract first number from string like "45 (Min/Max 20/46)" or "45"
	re := regexp.MustCompile(`(\d+)`)
	if matches := re.FindStringSubmatch(str); len(matches) >= 2 {
		if val, err := strconv.Atoi(matches[1]); err == nil {
			return val
		}
	}
	return 0
}

func getNVMeTemperature(device string) int {
	// Try NVMe specific temperature command
	cmd := exec.Command("smartctl", "-A", device)
	output, err := cmd.Output()
	if err != nil {
		return 0
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		// Look for NVMe temperature sensor
		if strings.Contains(line, "Temperature Sensor") || strings.Contains(line, "Composite") {
			// Format: "Temperature Sensor 1:           45 Celsius"
			re := regexp.MustCompile(`(\d+)\s+Celsius`)
			if matches := re.FindStringSubmatch(line); len(matches) >= 2 {
				if val, err := strconv.Atoi(matches[1]); err == nil && val > 0 && val < 150 {
					return val
				}
			}
		}

		// Alternative NVMe format: "Current Temperature:                    45 Celsius"
		if strings.Contains(line, "Current Temperature") {
			re := regexp.MustCompile(`(\d+)\s+Celsius`)
			if matches := re.FindStringSubmatch(line); len(matches) >= 2 {
				if val, err := strconv.Atoi(matches[1]); err == nil && val > 0 && val < 150 {
					return val
				}
			}
		}
	}

	return 0
}

func enrichDiskWithSMART(disk *DiskInfo) {
	if disk.SlotType == "USB" {
		// USB devices typically don't support SMART
		disk.Health = "N/A"
		return
	}

	// Get SMART health
	disk.Health = getSMARTHealth(disk.Device)

	// Get temperature with improved detection
	disk.Temperature = getSMARTTemperature(disk.Device)

	if debugMode && disk.Temperature == 0 {
		printDebug(fmt.Sprintf("Failed to detect temperature for %s (%s), trying manual smartctl check", disk.Device, disk.Model))

		// In debug mode, show what smartctl actually returns
		cmd := exec.Command("smartctl", "-A", disk.Device)
		if output, err := cmd.Output(); err == nil {
			lines := strings.Split(string(output), "\n")
			printDebug("Available SMART attributes:")
			for _, line := range lines {
				if strings.Contains(strings.ToLower(line), "temp") {
					printDebug(fmt.Sprintf("  %s", line))
				}
			}
		}
	}
}

func getSMARTTemperature(device string) int {
	cmd := exec.Command("smartctl", "-A", device)
	output, err := cmd.Output()
	if err != nil {
		// Try with different options for some devices
		cmd = exec.Command("smartctl", "-a", device)
		if output2, err2 := cmd.Output(); err2 == nil {
			output = output2
		} else {
			return 0
		}
	}

	outputStr := string(output)
	lines := strings.Split(outputStr, "\n")

	if debugMode {
		printDebug(fmt.Sprintf("SMART output for %s:", device))
		for _, line := range lines {
			if strings.Contains(strings.ToLower(line), "temp") {
				printDebug(fmt.Sprintf("  Temperature line: %s", line))
			}
		}
	}

	// Try multiple temperature detection methods
	temp := parseTemperatureFromLines(lines)
	if temp > 0 {
		return temp
	}

	// If standard parsing failed, try NVMe specific command
	if strings.Contains(device, "nvme") {
		return getNVMeTemperature(device)
	}

	return 0
}

func parseSlotRange(rangeStr string) (int, int, error) {
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
	var rows []RowConfig

	if totalSlots <= 4 {
		rows = append(rows, RowConfig{
			Name:  "Disk Slots",
			Slots: fmt.Sprintf("1-%d", totalSlots),
		})
	} else if totalSlots <= 8 {
		mid := totalSlots / 2
		rows = append(rows, RowConfig{
			Name:  "Primary Disks",
			Slots: fmt.Sprintf("1-%d", mid),
		})
		rows = append(rows, RowConfig{
			Name:  "Secondary Disks",
			Slots: fmt.Sprintf("%d-%d", mid+1, totalSlots),
		})
	} else {
		// Large configurations: break into rows of 6
		slotsPerRow := 6
		for i := 0; i < totalSlots; i += slotsPerRow {
			end := i + slotsPerRow
			if end > totalSlots {
				end = totalSlots
			}
			rows = append(rows, RowConfig{
				Name:  fmt.Sprintf("Disk Bank %d", len(rows)+1),
				Slots: fmt.Sprintf("%d-%d", i+1, end),
			})
		}
	}

	return CustomRowsConfig{
		Enabled: false,
		Rows:    rows,
	}
}

func createDefaultConfig(configPath string) error {
	printInfo("Scanning system for disk information to create configuration...")

	disks, err := getDiskInfo()
	if err != nil {
		return fmt.Errorf("could not scan disks: %v", err)
	}

	if len(disks) == 0 {
		return fmt.Errorf("no disk slots found - cannot create configuration")
	}

	printInfo(fmt.Sprintf("Found %d disk slot(s), creating configuration:", len(disks)))

	installedDisks := 0
	var diskTypes []string
	var slotTypes []string
	slotMapping := make(map[string]int)

	for i, disk := range disks {
		if disk.IsPresent {
			installedDisks++
			if disk.SlotType == "USB" {
				printInfo(fmt.Sprintf("  Slot %s (USB %s): %s %s %dGB [Bus:%s Device:%s Port:%s]",
					disk.Slot, disk.USBVersion, disk.DiskType, disk.Model, disk.SizeGB,
					disk.USBBus, disk.USBDevice, disk.USBPort))
			} else {
				printInfo(fmt.Sprintf("  Slot %s (%s): %s %s %dGB",
					disk.Slot, disk.SlotType, disk.DiskType, disk.Model, disk.SizeGB))
			}
			if disk.Temperature > 0 {
				printInfo(fmt.Sprintf("    Temperature: %d°C", disk.Temperature))
			}
			if disk.Health != "N/A" {
				printInfo(fmt.Sprintf("    Health: %s", disk.Health))
			}

			if disk.DiskType != "" && disk.DiskType != "Unknown" {
				diskTypes = append(diskTypes, disk.DiskType)
			}
			slotTypes = append(slotTypes, disk.SlotType)
		} else {
			printInfo(fmt.Sprintf("  Slot %s (%s): Empty", disk.Slot, disk.SlotType))
		}

		slotMapping[disk.Slot] = i + 1
	}

	printInfo(fmt.Sprintf("Total installed disks: %d", installedDisks))

	// Group disks by slot type, and USB devices by version
	slotTypeGroups := make(map[string][]DiskInfo)
	for _, disk := range disks {
		groupKey := disk.SlotType

		// For USB devices, create separate groups by version
		if disk.SlotType == "USB" && disk.USBVersion != "" {
			groupKey = fmt.Sprintf("USB_%s", disk.USBVersion)
		}

		slotTypeGroups[groupKey] = append(slotTypeGroups[groupKey], disk)
	}

	var requirements []DiskRequirement
	for groupKey, disksOfType := range slotTypeGroups {
		installedOfType := 0
		var requiredSlots []string

		// Determine the actual slot type and USB version
		slotType := "Unknown"
		usbVersion := ""

		if strings.HasPrefix(groupKey, "USB_") {
			slotType = "USB"
			usbVersion = strings.TrimPrefix(groupKey, "USB_")
		} else {
			slotType = groupKey
		}

		for _, disk := range disksOfType {
			if disk.IsPresent {
				installedOfType++
				requiredSlots = append(requiredSlots, disk.Slot)
			}
		}

		if installedOfType > 0 {
			var reqName string
			if usbVersion != "" {
				reqName = fmt.Sprintf("USB %s disks (%d installed)", usbVersion, installedOfType)
			} else {
				reqName = fmt.Sprintf("%s disks (%d installed)", slotType, installedOfType)
			}

			req := DiskRequirement{
				Name:          reqName,
				SlotType:      slotType,
				RequiredSlots: requiredSlots,
				MinDisks:      installedOfType,
				MinSizeGB:     8,                 // Minimum 8GB
				MaxTempC:      70,                // Maximum 70°C
				CheckSMART:    slotType != "USB", // No SMART for USB
			}

			// Add USB version requirement for USB devices
			if slotType == "USB" && usbVersion != "" {
				req.USBVersion = usbVersion
				printInfo(fmt.Sprintf("  USB %s version requirement created", usbVersion))
			}

			requirements = append(requirements, req)
		}
	}

	// Create type visuals
	typeVisuals := make(map[string]DiskVisual)
	typeVisuals["SSD"] = DiskVisual{
		Symbol:      "▓▓▓",
		ShortName:   "SSD",
		Description: "Solid State Drive",
		Color:       "green",
	}
	typeVisuals["NVMe"] = DiskVisual{
		Symbol:      "=*=",
		ShortName:   "NVME",
		Description: "NVMe SSD",
		Color:       "cyan",
	}
	typeVisuals["HDD"] = DiskVisual{
		Symbol:      "█=█",
		ShortName:   "HDD",
		Description: "Hard Disk Drive",
		Color:       "blue",
	}
	typeVisuals["USB"] = DiskVisual{
		Symbol:      "---",
		ShortName:   "USB",
		Description: "USB Storage",
		Color:       "yellow",
	}
	typeVisuals["Unknown"] = DiskVisual{
		Symbol:      "░░░",
		ShortName:   "UNK",
		Description: "Unknown Type",
		Color:       "gray",
	}

	config := Config{
		DiskRequirements: requirements,
		Visualization: VisualizationConfig{
			TypeVisuals: typeVisuals,
			SlotMapping: slotMapping,
			TotalSlots:  len(disks),
			SlotWidth:   12, // Increase width for USB info
			SlotsPerRow: 6,
			CustomRows:  generateDefaultCustomRows(len(disks)),
		},
		CheckTemperature: true,
		CheckSMART:       true,
		SMARTTimeout:     10,
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
	printInfo(fmt.Sprintf("Total disk slots: %d", len(disks)))
	printInfo(fmt.Sprintf("Installed disks: %d", installedDisks))
	printInfo("USB devices grouped by version for accurate requirements")
	printInfo("Custom row layout generated (disabled by default)")
	printInfo("To enable custom rows: set 'visualization.custom_rows.enabled' to true")

	return nil
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

	// Set defaults
	if cfg.SMARTTimeout == 0 {
		cfg.SMARTTimeout = 10
	}

	return &cfg, nil
}

func checkDiskAgainstRequirements(disks []DiskInfo, config *Config) DiskCheckResult {
	result := DiskCheckResult{
		Status:   "ok",
		SizeOK:   true,
		TempOK:   true,
		TypeOK:   true,
		HealthOK: true,
	}

	if len(config.DiskRequirements) == 0 {
		return result
	}

	hasErrors := false
	hasWarnings := false

	for _, req := range config.DiskRequirements {
		matchingDisks := filterDisks(disks, req)
		installedCount := 0

		for _, disk := range matchingDisks {
			if disk.IsPresent {
				installedCount++
			}
		}

		// Check disk count
		if req.MinDisks > 0 && installedCount < req.MinDisks {
			result.Issues = append(result.Issues,
				fmt.Sprintf("%s: found %d disk(s), required %d", req.Name, installedCount, req.MinDisks))
			hasErrors = true
		}

		if req.MaxDisks > 0 && installedCount > req.MaxDisks {
			result.Issues = append(result.Issues,
				fmt.Sprintf("%s: found %d disk(s), maximum %d", req.Name, installedCount, req.MaxDisks))
			hasErrors = true
		}

		// Check individual disk requirements
		for _, disk := range matchingDisks {
			if !disk.IsPresent {
				continue
			}

			// Check size
			if req.MinSizeGB > 0 && disk.SizeGB < req.MinSizeGB {
				result.Issues = append(result.Issues,
					fmt.Sprintf("Disk %s: %dGB (required %dGB)", disk.Slot, disk.SizeGB, req.MinSizeGB))
				result.SizeOK = false
				hasErrors = true
			}

			// Check temperature
			if config.CheckTemperature && req.MaxTempC > 0 && disk.Temperature > 0 {
				if disk.Temperature > req.MaxTempC {
					result.Issues = append(result.Issues,
						fmt.Sprintf("Disk %s: %d°C (max %d°C)", disk.Slot, disk.Temperature, req.MaxTempC))
					result.TempOK = false
					hasErrors = true
				}
			}

			// Check type
			if req.RequiredType != "" && disk.DiskType != req.RequiredType {
				result.Issues = append(result.Issues,
					fmt.Sprintf("Disk %s: %s (required %s)", disk.Slot, disk.DiskType, req.RequiredType))
				result.TypeOK = false
				hasErrors = true
			}

			// Check USB version for USB devices
			if req.SlotType == "USB" && req.USBVersion != "" && disk.USBVersion != req.USBVersion {
				result.Issues = append(result.Issues,
					fmt.Sprintf("USB disk %s: USB %s (required USB %s)", disk.Slot, disk.USBVersion, req.USBVersion))
				result.TypeOK = false
				hasErrors = true
			}

			// Check SMART health
			if config.CheckSMART && req.CheckSMART && disk.Health == "FAILING" {
				result.Issues = append(result.Issues,
					fmt.Sprintf("Disk %s: SMART health failing", disk.Slot))
				result.HealthOK = false
				hasErrors = true
			}
		}

		// Check required slots
		for _, reqSlot := range req.RequiredSlots {
			found := false
			for _, disk := range disks {
				if disk.Slot == reqSlot && disk.IsPresent {
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

func filterDisks(disks []DiskInfo, req DiskRequirement) []DiskInfo {
	var matching []DiskInfo

	for _, disk := range disks {
		// Check slot type
		if req.SlotType != "" && req.SlotType != "any" && disk.SlotType != req.SlotType {
			continue
		}

		// For USB devices, check version if specified
		if req.SlotType == "USB" && req.USBVersion != "" && disk.USBVersion != req.USBVersion {
			continue
		}

		// Check specific required slots
		if len(req.RequiredSlots) > 0 {
			found := false
			for _, slot := range req.RequiredSlots {
				if disk.Slot == slot {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		matching = append(matching, disk)
	}

	return matching
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

func formatSize(sizeGB int) string {
	if sizeGB == 0 {
		return "Empty"
	}
	if sizeGB < 1024 {
		return fmt.Sprintf("%dGB", sizeGB)
	} else {
		return fmt.Sprintf("%.1fTB", float64(sizeGB)/1024.0)
	}
}

func formatTemp(temp int) string {
	if temp == 0 {
		return "N/A"
	}
	return fmt.Sprintf("%d°C", temp)
}

func getExpectedUSBVersion(slotName string, requirements []DiskRequirement) string {
	for _, req := range requirements {
		if req.SlotType == "USB" && req.USBVersion != "" {
			for _, reqSlot := range req.RequiredSlots {
				if reqSlot == slotName {
					return req.USBVersion
				}
			}
		}
	}
	return ""
}

func shortenSlotName(slot string) string {
	// For USB slots like "USB:001-1-2:3.0", show as "USB1-2:3.0"
	if strings.HasPrefix(slot, "USB:") {
		parts := strings.Split(slot, ":")
		if len(parts) == 3 {
			busPort := parts[1] // "001-1-2"
			version := parts[2] // "3.0"

			// Extract port part (remove bus prefix)
			if strings.Contains(busPort, "-") {
				portParts := strings.Split(busPort, "-")
				if len(portParts) >= 2 {
					port := strings.Join(portParts[1:], "-") // "1-2"
					return fmt.Sprintf("USB%s:%s", port, version)
				}
			}
			return fmt.Sprintf("USB:%s", version)
		}
	}

	// For HCTL addresses like "1:0:0:0", show as "1:0:0:0" or abbreviated
	if regexp.MustCompile(`^\d+:\d+:\d+:\d+$`).MatchString(slot) {
		parts := strings.Split(slot, ":")
		if len(parts) == 4 {
			return fmt.Sprintf("%s:%s", parts[0], parts[3]) // Show H:L
		}
	}

	// For PCI addresses like "0000:06:00.0", show as "06:00.0"
	if regexp.MustCompile(`^\d{4}:\d{2}:\d{2}\.\d$`).MatchString(slot) {
		parts := strings.Split(slot, ":")
		if len(parts) == 3 {
			return strings.Join(parts[1:], ":") // Remove domain
		}
	}

	// Fallback: take first 10 characters
	if len(slot) > 10 {
		return slot[:10]
	}
	return slot
}

func visualizeSlots(disks []DiskInfo, config *Config) error {
	printInfo("Disk Slots Layout:")
	fmt.Println()

	maxSlots := config.Visualization.TotalSlots
	if maxSlots == 0 {
		maxSlots = len(disks)
	}

	// Collect required slots
	required := make(map[string]bool)
	for _, req := range config.DiskRequirements {
		for _, slot := range req.RequiredSlots {
			required[slot] = true
		}
	}

	// Create position to slot mapping
	posToSlot := make(map[int]string, len(config.Visualization.SlotMapping))
	for slot, pos := range config.Visualization.SlotMapping {
		posToSlot[pos] = slot
	}

	// Fill slot data array
	slotData := make([]DiskInfo, maxSlots+1)
	for _, disk := range disks {
		if pos, ok := config.Visualization.SlotMapping[disk.Slot]; ok && pos >= 1 && pos <= maxSlots {
			slotData[pos] = disk
		}
	}

	// System check for coloring
	systemResult := checkDiskAgainstRequirements(disks, config)

	// Legend
	printInfo("Legend:")
	fmt.Printf("  %s%s%s Present      ", ColorGreen, "▓▓▓", ColorReset)
	fmt.Printf("  %s%s%s Issues       ", ColorYellow, "▓▓▓", ColorReset)
	fmt.Printf("  %sMISS%s Missing Req", ColorRed, ColorReset)
	fmt.Printf("  %s%s%s Empty Slot\n", ColorWhite, "░░░", ColorReset)
	fmt.Println()

	// Generate rows
	var rows []RowConfig
	if config.Visualization.CustomRows.Enabled && len(config.Visualization.CustomRows.Rows) > 0 {
		rows = config.Visualization.CustomRows.Rows
	} else {
		perRow := config.Visualization.SlotsPerRow
		if perRow == 0 {
			perRow = 6
		}
		for start := 1; start <= maxSlots; start += perRow {
			end := start + perRow - 1
			if end > maxSlots {
				end = maxSlots
			}
			rows = append(rows, RowConfig{
				Name:  fmt.Sprintf("Disk Bank %d", len(rows)+1),
				Slots: fmt.Sprintf("%d-%d", start, end),
			})
		}
	}

	// Visualize each row
	for _, row := range rows {
		start, end, err := parseSlotRange(row.Slots)
		if err != nil || start < 1 || end > maxSlots {
			printWarning(fmt.Sprintf("Skipping invalid row '%s': %v", row.Slots, err))
			continue
		}
		count := end - start + 1
		width := config.Visualization.SlotWidth
		if width < 4 {
			width = 4
		}

		// Row header
		if len(rows) > 1 {
			fmt.Printf("%s (Slots %d-%d):\n", row.Name, start, end)
		}

		// Top border
		fmt.Print("┌")
		for i := 0; i < count; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < count-1 {
				fmt.Print("┬")
			}
		}
		fmt.Println("┐")

		// Symbols row
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			disk := slotData[idx]

			if disk.IsPresent {
				vis := getDiskVisual(disk, &config.Visualization)
				sym := centerText(vis.Symbol, width)
				color := getANSIColor(vis.Color)
				fmt.Print(color + sym + ColorReset)
			} else {
				slotName := posToSlot[idx]
				if required[slotName] {
					miss := centerText("MISS", width)
					fmt.Print(ColorRed + miss + ColorReset)
				} else {
					fmt.Print(centerText("░░░", width))
				}
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Type row
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			disk := slotData[idx]
			if disk.IsPresent {
				vis := getDiskVisual(disk, &config.Visualization)
				txt := centerText(vis.ShortName, width)
				if systemResult.Status == "error" {
					fmt.Print(ColorYellow + txt + ColorReset)
				} else {
					fmt.Print(ColorGreen + txt + ColorReset)
				}
			} else {
				slotName := posToSlot[idx]
				if required[slotName] {
					// Show expected type for missing required slot
					expectedUSBVer := getExpectedUSBVersion(slotName, config.DiskRequirements)
					if expectedUSBVer != "" {
						txt := centerText("USB", width)
						fmt.Print(ColorRed + txt + ColorReset)
					} else {
						txt := centerText("MISS", width)
						fmt.Print(ColorRed + txt + ColorReset)
					}
				} else {
					fmt.Print(strings.Repeat(" ", width))
				}
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Size row
		fmt.Print("│")
		for i := 0; i < count; i++ {
			idx := start + i
			disk := slotData[idx]
			if disk.IsPresent {
				txt := centerText(formatSize(disk.SizeGB), width)
				if systemResult.Status == "error" {
					fmt.Print(ColorYellow + txt + ColorReset)
				} else {
					fmt.Print(ColorGreen + txt + ColorReset)
				}
			} else {
				fmt.Print(strings.Repeat(" ", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// USB Version row (for USB devices and expected USB)
		hasUSBOrExpected := false
		for i := 0; i < count; i++ {
			idx := start + i
			disk := slotData[idx]
			slotName := posToSlot[idx]
			expectedUSBVer := getExpectedUSBVersion(slotName, config.DiskRequirements)

			if (disk.IsPresent && disk.SlotType == "USB") || expectedUSBVer != "" {
				hasUSBOrExpected = true
				break
			}
		}

		if hasUSBOrExpected {
			fmt.Print("│")
			for i := 0; i < count; i++ {
				idx := start + i
				disk := slotData[idx]
				slotName := posToSlot[idx]
				expectedUSBVer := getExpectedUSBVersion(slotName, config.DiskRequirements)

				if disk.IsPresent && disk.SlotType == "USB" {
					txt := centerText("USB "+disk.USBVersion, width)
					if systemResult.Status == "error" {
						fmt.Print(ColorYellow + txt + ColorReset)
					} else {
						fmt.Print(ColorGreen + txt + ColorReset)
					}
				} else if expectedUSBVer != "" {
					// Show expected USB version for missing USB slot
					txt := centerText("USB "+expectedUSBVer, width)
					fmt.Print(ColorRed + txt + ColorReset)
				} else {
					fmt.Print(strings.Repeat(" ", width))
				}
				fmt.Print("│")
			}
			fmt.Println()
		}

		// Temperature row (if enabled)
		if config.CheckTemperature {
			fmt.Print("│")
			for i := 0; i < count; i++ {
				idx := start + i
				disk := slotData[idx]
				if disk.IsPresent {
					txt := centerText(formatTemp(disk.Temperature), width)
					if systemResult.Status == "error" {
						fmt.Print(ColorYellow + txt + ColorReset)
					} else {
						fmt.Print(ColorGreen + txt + ColorReset)
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
		for i := 0; i < count; i++ {
			fmt.Print(strings.Repeat("─", width))
			if i < count-1 {
				fmt.Print("┴")
			}
		}
		fmt.Println("┘")

		// Slot numbers
		fmt.Print(" ")
		for i := 0; i < count; i++ {
			fmt.Print(centerText(fmt.Sprintf("%d", start+i), width+1))
		}
		fmt.Println(" (Slot)")

		// Slot addresses
		fmt.Print(" ")
		for i := 0; i < count; i++ {
			name := posToSlot[start+i]
			short := shortenSlotName(name)
			fmt.Print(centerText(short, width+1))
		}
		fmt.Printf(" (Address)\n\n")
	}

	// Final status
	switch systemResult.Status {
	case "error":
		printError("Disk configuration validation FAILED!")
		return fmt.Errorf("validation failed")
	case "warning":
		printWarning("Validation completed with warnings")
	default:
		printSuccess("All disk slots meet requirements!")
	}
	return nil
}

func getDiskVisual(disk DiskInfo, config *VisualizationConfig) DiskVisual {
	if !disk.IsPresent {
		return DiskVisual{
			Symbol:      "░░░",
			ShortName:   "",
			Description: "Empty Slot",
			Color:       "gray",
		}
	}

	diskType := disk.DiskType
	if diskType == "" {
		diskType = "Unknown"
	}

	if visual, exists := config.TypeVisuals[diskType]; exists {
		return visual
	}

	// Fallback: Unknown
	return DiskVisual{
		Symbol:      "???",
		ShortName:   diskType,
		Description: diskType + " Drive",
		Color:       "white",
	}
}

func checkDisk(config *Config) error {
	printInfo("Starting disk check...")

	disks, err := getDiskInfo()
	if err != nil {
		return fmt.Errorf("failed to get disk info: %v", err)
	}

	printInfo(fmt.Sprintf("Found disk slots: %d", len(disks)))

	if len(disks) == 0 {
		printError("No disk slots found")
		return fmt.Errorf("no disk slots found")
	}

	// Display found disks
	installedCount := 0
	for i, disk := range disks {
		if disk.IsPresent {
			installedCount++
			if disk.SlotType == "USB" {
				printInfo(fmt.Sprintf("Slot %d (%s): %s %s %s [USB %s Bus:%s Device:%s]",
					i+1, disk.Slot, disk.DiskType, disk.Model, formatSize(disk.SizeGB),
					disk.USBVersion, disk.USBBus, disk.USBDevice))
			} else {
				printInfo(fmt.Sprintf("Slot %d (%s): %s %s %s", i+1, disk.Slot, disk.DiskType, disk.Model, formatSize(disk.SizeGB)))
			}
			if disk.Temperature > 0 {
				printDebug(fmt.Sprintf("  Temperature: %d°C", disk.Temperature))
			}
			if disk.Health != "N/A" {
				printDebug(fmt.Sprintf("  Health: %s", disk.Health))
			}
		} else {
			printInfo(fmt.Sprintf("Slot %d (%s): Empty", i+1, disk.Slot))
		}
	}

	printInfo(fmt.Sprintf("Total installed: %d disks", installedCount))

	// Check requirements
	result := checkDiskAgainstRequirements(disks, config)

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
		printError("Disk requirements FAILED")
		return fmt.Errorf("disk requirements not met")
	} else if result.Status == "warning" {
		printWarning("Disk requirements passed with warnings")
	} else {
		printSuccess("All disk requirements passed")
	}

	return nil
}

func main() {
	var (
		showVersion  = flag.Bool("V", false, "Show version")
		configPath   = flag.String("c", "disk_config.json", "Path to configuration file")
		createConfig = flag.Bool("s", false, "Create default configuration file")
		showHelpFlag = flag.Bool("h", false, "Show help")
		listOnly     = flag.Bool("l", false, "List detected disks without configuration check")
		visualize    = flag.Bool("vis", false, "Show visual disk slots layout")
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
		printInfo("Scanning for disk information...")
		disks, err := getDiskInfo()
		if err != nil {
			printError(fmt.Sprintf("Error getting disk information: %v", err))
			os.Exit(1)
		}

		if len(disks) == 0 {
			printWarning("No disk slots found")
		} else {
			printSuccess(fmt.Sprintf("Found disk slots: %d", len(disks)))
			installedCount := 0

			for i, disk := range disks {
				fmt.Printf("\nSlot %d:\n", i+1)
				fmt.Printf("  Slot Address: %s\n", disk.Slot)
				fmt.Printf("  Slot Type: %s\n", disk.SlotType)
				fmt.Printf("  Present: %t\n", disk.IsPresent)
				if disk.IsPresent {
					fmt.Printf("  Device: %s\n", disk.Device)
					fmt.Printf("  Type: %s\n", disk.DiskType)
					fmt.Printf("  Model: %s\n", disk.Model)
					fmt.Printf("  Size: %s\n", formatSize(disk.SizeGB))
					fmt.Printf("  Serial: %s\n", disk.Serial)
					fmt.Printf("  Temperature: %s\n", formatTemp(disk.Temperature))
					fmt.Printf("  Health: %s\n", disk.Health)

					// USB specific information
					if disk.SlotType == "USB" {
						fmt.Printf("  USB Bus: %s\n", disk.USBBus)
						fmt.Printf("  USB Device: %s\n", disk.USBDevice)
						fmt.Printf("  USB Version: %s\n", disk.USBVersion)
						fmt.Printf("  USB Port: %s\n", disk.USBPort)
					}

					installedCount++
				}
			}

			fmt.Printf("\nSummary:\n")
			fmt.Printf("  Total slots: %d\n", len(disks))
			fmt.Printf("  Installed disks: %d\n", installedCount)
		}
		return
	}

	if *visualize {
		printInfo("Scanning for disk information...")
		disks, err := getDiskInfo()
		if err != nil {
			printError(fmt.Sprintf("Error getting disk information: %v", err))
			os.Exit(1)
		}

		config, err := loadConfig(*configPath)
		if err != nil {
			printError(fmt.Sprintf("Error loading configuration: %v", err))
			printInfo("Use -s to create a default configuration file")
			os.Exit(1)
		}

		err = visualizeSlots(disks, config)
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
		printInfo("Or use -l to simply display found disks")
		os.Exit(1)
	}

	printInfo(fmt.Sprintf("Configuration loaded from: %s", *configPath))

	err = checkDisk(config)
	if err != nil {
		printError(fmt.Sprintf("Disk check failed: %v", err))
		os.Exit(1)
	}
}
