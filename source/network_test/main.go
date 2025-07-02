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
	"sync"
	"time"

	"github.com/safchain/ethtool"
	"github.com/vishvananda/netlink"
)

const VERSION = "1.0.1"

type NetworkInterface struct {
	Name      string `json:"name"`       // Interface name (eth0, ens32, etc.)
	MAC       string `json:"mac"`        // MAC address
	IP        string `json:"ip"`         // Current IP address
	Driver    string `json:"driver"`     // Network driver
	State     string `json:"state"`      // UP, DOWN
	Link      string `json:"link"`       // Link status (up, down)
	Speed     string `json:"speed"`      // Link speed (1000Mb/s, etc.)
	PCISlot   string `json:"pci_slot"`   // PCI address if available
	IsPresent bool   `json:"is_present"` // Whether interface exists
	Type      string `json:"type"`       // Ethernet, WiFi, Loopback, etc.
}

type NetworkRequirement struct {
	Name                string            `json:"name"`
	MinInterfaces       int               `json:"min_interfaces"`        // Minimum number of interfaces
	MaxInterfaces       int               `json:"max_interfaces"`        // Maximum number of interfaces
	RequiredInterfaces  []string          `json:"required_interfaces"`   // Specific interface names that must exist
	RequiredDrivers     []string          `json:"required_drivers"`      // Required drivers
	RequiredType        string            `json:"required_type"`         // Required interface type
	CheckPing           bool              `json:"check_ping"`            // Whether to test ping
	PingTargets         []string          `json:"ping_targets"`          // Targets to ping per interface
	PingTimeout         int               `json:"ping_timeout_seconds"`  // Ping timeout
	RequiredSpeed       string            `json:"required_speed"`        // Required link speed
	RequireLink         bool              `json:"require_link"`          // Require link to be up
	ExpectedStates      map[string]string `json:"expected_states"`       // interface -> expected state
	AllowedInterfaceIPs []string          `json:"allowed_interface_ips"` // Allowed IP ranges
}

type NetworkVisual struct {
	Symbol      string `json:"symbol"`
	ShortName   string `json:"short_name"`
	Description string `json:"description"`
	Color       string `json:"color"`
}

type RowConfig struct {
	Name       string `json:"name"`       // Display name for the row
	Interfaces string `json:"interfaces"` // Interface range or list
}

type CustomRowsConfig struct {
	Enabled bool        `json:"enabled"` // Enable custom row configuration
	Rows    []RowConfig `json:"rows"`    // Custom row definitions
}

type VisualizationConfig struct {
	TypeVisuals      map[string]NetworkVisual `json:"type_visuals"`       // interface type -> visual
	InterfaceMapping map[string]int           `json:"interface_mapping"`  // interface name -> logical position
	TotalSlots       int                      `json:"total_slots"`        // Total interface slots
	SlotWidth        int                      `json:"slot_width"`         // Width of each slot in visualization
	InterfacesPerRow int                      `json:"interfaces_per_row"` // Number of interfaces per row (legacy)
	CustomRows       CustomRowsConfig         `json:"custom_rows"`        // Custom row configuration
}

type Config struct {
	NetworkRequirements []NetworkRequirement `json:"network_requirements"`
	Visualization       VisualizationConfig  `json:"visualization"`
	CheckPing           bool                 `json:"check_ping"`
	CheckSpeed          bool                 `json:"check_speed"`
	CheckLink           bool                 `json:"check_link"`
	PingTimeout         int                  `json:"ping_timeout_seconds"`
	PingRetries         int                  `json:"ping_retries"` // Number of retry attempts on ping failure
}

type NetworkCheckResult struct {
	Status     string // "ok", "warning", "error", "missing"
	Issues     []string
	StateOK    bool
	SpeedOK    bool
	LinkOK     bool
	PingOK     bool
	DriverOK   bool
	StateWarn  bool
	SpeedWarn  bool
	LinkWarn   bool
	PingWarn   bool
	DriverWarn bool
}

type PingResult struct {
	Target    string
	Success   bool
	Loss      float64 // Packet loss percentage
	AvgTime   float64 // Average response time in ms
	Error     string
	Interface string
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
var outputMutex sync.Mutex

func printColoredSafe(color, message string) {
	outputMutex.Lock()
	defer outputMutex.Unlock()
	fmt.Printf("%s%s%s\n", color, message, ColorReset)
}

func printSuccessSafe(message string) {
	printColoredSafe(ColorGreen, message)
}

func printInfoSafe(message string) {
	printColoredSafe(ColorBlue, message)
}

func printDebugSafe(message string) {
	if debugMode {
		printColoredSafe(ColorWhite, message)
	}
}

func printWarningSafe(message string) {
	printColoredSafe(ColorYellow, message)
}

func printErrorSafe(message string) {
	printColoredSafe(ColorRed, message)
}

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
	fmt.Printf("Network Interface Checker %s\n", VERSION)
	fmt.Println("Parameters:")
	fmt.Println("  -V          Show program version")
	fmt.Println("  -c <path>   Path to configuration file")
	fmt.Println("  -s          Create default configuration file")
	fmt.Println("  -l          List detected network interfaces without configuration check")
	fmt.Println("  -vis        Show visual network interfaces layout")
	fmt.Println("  -ping       Test ping on all interfaces (requires configuration)")
	fmt.Println("  -d          Show detailed debug information")
	fmt.Println("  -h          Show this help")
}

func getNetworkInterfaces() ([]NetworkInterface, error) {
	var interfaces []NetworkInterface

	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("netlink failed: %v", err)
	}

	ethTool, err := ethtool.NewEthtool()
	if err != nil {
		printWarningSafe("ethtool not available, skipping driver/speed info")
		ethTool = nil
	}

	for _, link := range links {
		attrs := link.Attrs()
		name := attrs.Name

		iface := NetworkInterface{
			Name:      name,
			MAC:       attrs.HardwareAddr.String(),
			IsPresent: true,
			Type:      determineInterfaceType(name),
			PCISlot:   "",
		}

		// State
		switch attrs.OperState {
		case netlink.OperUp:
			iface.State = "UP"
		case netlink.OperDown:
			iface.State = "DOWN"
		default:
			iface.State = "UNKNOWN"
		}

		// IP
		addrList, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err == nil && len(addrList) > 0 {
			iface.IP = addrList[0].IP.String()
		} else {
			iface.IP = ""
		}

		// Driver
		if ethTool != nil {
			if driver, err := ethTool.DriverName(name); err == nil {
				iface.Driver = driver
			}
		}

		// Speed
		speedPath := fmt.Sprintf("/sys/class/net/%s/speed", name)
		if data, err := os.ReadFile(speedPath); err == nil {
			s := strings.TrimSpace(string(data))
			if sp, err := strconv.Atoi(s); err == nil && sp >= 0 {
				iface.Speed = fmt.Sprintf("%dMb/s", sp)
			} else {
				iface.Speed = "unknown"
			}
		} else if ethTool != nil {
			// fallback: старая логика ethtool.Stats, если sysfs недоступен
			stats, err := ethTool.Stats(name)
			if err == nil {
				if speedVal, ok := stats["Speed"]; ok && speedVal > 0 {
					iface.Speed = fmt.Sprintf("%dMb/s", speedVal)
				} else {
					iface.Speed = "unknown"
				}
			} else {
				iface.Speed = "unknown"
			}
		} else {
			iface.Speed = "unknown"
		}

		// Link
		if attrs.OperState == netlink.OperUp {
			iface.Link = "up"
		} else {
			iface.Link = "down"
		}

		// PCI Slot (fallback sysfs)
		if pciSlot, err := getInterfacePCISlot(name); err == nil {
			iface.PCISlot = pciSlot
		}

		if debugMode {
			printDebugSafe(fmt.Sprintf("Interface %s: MAC=%s IP=%s State=%s Driver=%s Speed=%s",
				iface.Name, iface.MAC, iface.IP, iface.State, iface.Driver, iface.Speed))
		}

		interfaces = append(interfaces, iface)
	}

	return interfaces, nil
}

func determineInterfaceType(name string) string {
	name = strings.ToLower(name)

	if name == "lo" {
		return "Loopback"
	} else if strings.HasPrefix(name, "wl") || strings.HasPrefix(name, "wlan") {
		return "WiFi"
	} else if strings.HasPrefix(name, "eth") || strings.HasPrefix(name, "ens") ||
		strings.HasPrefix(name, "eno") || strings.HasPrefix(name, "enp") {
		return "Ethernet"
	} else if strings.HasPrefix(name, "br") {
		return "Bridge"
	} else if strings.HasPrefix(name, "veth") {
		return "Virtual"
	} else if strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "virbr") {
		return "Virtual"
	}

	return "Other"
}

func getInterfacePCISlot(interfaceName string) (string, error) {
	// Get PCI slot from /sys/class/net
	devicePath := fmt.Sprintf("/sys/class/net/%s/device", interfaceName)
	if realPath, err := filepath.EvalSymlinks(devicePath); err == nil {
		pciRegex := regexp.MustCompile(`(\d{4}:\d{2}:\d{2}\.\d)`)
		if matches := pciRegex.FindStringSubmatch(realPath); len(matches) >= 2 {
			return matches[1], nil
		}
	}
	return "", fmt.Errorf("PCI slot not found for interface %s", interfaceName)
}

func generateDefaultCustomRows(totalInterfaces int) CustomRowsConfig {
	var rows []RowConfig

	if totalInterfaces <= 4 {
		rows = append(rows, RowConfig{
			Name:       "Network Interfaces",
			Interfaces: "1-" + strconv.Itoa(totalInterfaces),
		})
	} else if totalInterfaces <= 8 {
		mid := totalInterfaces / 2
		rows = append(rows, RowConfig{
			Name:       "Primary Interfaces",
			Interfaces: "1-" + strconv.Itoa(mid),
		})
		rows = append(rows, RowConfig{
			Name:       "Secondary Interfaces",
			Interfaces: strconv.Itoa(mid+1) + "-" + strconv.Itoa(totalInterfaces),
		})
	} else {
		// Large configurations: break into rows of 6
		interfacesPerRow := 6
		for i := 0; i < totalInterfaces; i += interfacesPerRow {
			end := i + interfacesPerRow
			if end > totalInterfaces {
				end = totalInterfaces
			}
			rows = append(rows, RowConfig{
				Name:       fmt.Sprintf("Network Bank %d", len(rows)+1),
				Interfaces: fmt.Sprintf("%d-%d", i+1, end),
			})
		}
	}

	return CustomRowsConfig{
		Enabled: false,
		Rows:    rows,
	}
}

func createDefaultConfig(configPath string) error {
	printInfo("Scanning system for network interfaces to create configuration...")

	interfaces, err := getNetworkInterfaces()
	if err != nil {
		return fmt.Errorf("could not scan network interfaces: %v", err)
	}

	if len(interfaces) == 0 {
		return fmt.Errorf("no network interfaces found - cannot create configuration")
	}

	// Filter out loopback and virtual interfaces for requirements
	var physicalInterfaces []NetworkInterface
	for _, iface := range interfaces {
		if iface.Type != "Loopback" && iface.Type != "Virtual" && iface.Type != "Bridge" {
			physicalInterfaces = append(physicalInterfaces, iface)
		}
	}

	printInfo(fmt.Sprintf("Found %d total interfaces (%d physical):", len(interfaces), len(physicalInterfaces)))

	interfaceMapping := make(map[string]int)

	for i, iface := range interfaces {
		if iface.Type == "Loopback" {
			printInfo(fmt.Sprintf("  %s: %s (loopback)", iface.Name, iface.Type))
		} else {
			printInfo(fmt.Sprintf("  %s: %s %s", iface.Name, iface.Type, iface.State))
			if iface.IP != "" {
				printInfo(fmt.Sprintf("    IP: %s", iface.IP))
			}
			if iface.MAC != "" {
				printInfo(fmt.Sprintf("    MAC: %s", iface.MAC))
			}
			if iface.Driver != "" {
				printInfo(fmt.Sprintf("    Driver: %s", iface.Driver))
			}
			if iface.Speed != "" && iface.Speed != "unknown" {
				printInfo(fmt.Sprintf("    Speed: %s", iface.Speed))
			}
		}
		interfaceMapping[iface.Name] = i + 1
	}

	// Group interfaces by type
	typeGroups := make(map[string][]NetworkInterface)
	for _, iface := range physicalInterfaces {
		typeGroups[iface.Type] = append(typeGroups[iface.Type], iface)
	}

	var requirements []NetworkRequirement
	for ifaceType, interfacesOfType := range typeGroups {
		var requiredInterfaces []string
		expectedStates := make(map[string]string)

		for _, iface := range interfacesOfType {
			requiredInterfaces = append(requiredInterfaces, iface.Name)
			expectedStates[iface.Name] = iface.State
		}

		req := NetworkRequirement{
			Name:               fmt.Sprintf("%s interfaces (%d found)", ifaceType, len(interfacesOfType)),
			MinInterfaces:      len(interfacesOfType),
			RequiredInterfaces: requiredInterfaces,
			RequiredType:       ifaceType,
			ExpectedStates:     expectedStates,
			CheckPing:          false, // Disabled by default
			PingTargets:        []string{"8.8.8.8", "1.1.1.1"},
			PingTimeout:        5,
			RequireLink:        false, // Disabled by default
		}
		requirements = append(requirements, req)
	}

	// Create type visuals
	typeVisuals := make(map[string]NetworkVisual)
	typeVisuals["Ethernet"] = NetworkVisual{
		Symbol:      "▓▓▓",
		ShortName:   "ETH",
		Description: "Ethernet Interface",
		Color:       "green",
	}
	typeVisuals["WiFi"] = NetworkVisual{
		Symbol:      "~~~",
		ShortName:   "WIFI",
		Description: "WiFi Interface",
		Color:       "cyan",
	}
	typeVisuals["Loopback"] = NetworkVisual{
		Symbol:      "○○○",
		ShortName:   "LO",
		Description: "Loopback Interface",
		Color:       "gray",
	}
	typeVisuals["Bridge"] = NetworkVisual{
		Symbol:      "═══",
		ShortName:   "BR",
		Description: "Bridge Interface",
		Color:       "blue",
	}
	typeVisuals["Virtual"] = NetworkVisual{
		Symbol:      "- -",
		ShortName:   "VIRT",
		Description: "Virtual Interface",
		Color:       "yellow",
	}
	typeVisuals["Other"] = NetworkVisual{
		Symbol:      "░░░",
		ShortName:   "OTH",
		Description: "Other Interface",
		Color:       "white",
	}

	config := Config{
		NetworkRequirements: requirements,
		Visualization: VisualizationConfig{
			TypeVisuals:      typeVisuals,
			InterfaceMapping: interfaceMapping,
			TotalSlots:       len(interfaces),
			SlotWidth:        10,
			InterfacesPerRow: 6,
			CustomRows:       generateDefaultCustomRows(len(interfaces)),
		},
		CheckPing:   false, // Disabled by default
		CheckSpeed:  true,
		CheckLink:   false, // Disabled by default
		PingTimeout: 5,
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

	printSuccess("Configuration created successfully based on detected interfaces")
	printInfo(fmt.Sprintf("Total interfaces: %d", len(interfaces)))
	printInfo(fmt.Sprintf("Physical interfaces: %d", len(physicalInterfaces)))
	printInfo("Ping testing disabled by default (can be enabled in config)")
	printInfo("Custom row layout generated (disabled by default)")

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
	if cfg.PingTimeout == 0 {
		cfg.PingTimeout = 5
	}

	return &cfg, nil
}

var (
	// паттерны для разбора строки packet loss и строки rtt
	lossRe = regexp.MustCompile(`, (\d+)% packet loss`)
	rttRe  = regexp.MustCompile(`rtt [^=]+= [\d\.]+/([\d\.]+)/`)
)

func pingInterface(interfaceName, target string, timeoutSec, retries int) PingResult {
	result := PingResult{
		Target:    target,
		Interface: interfaceName,
	}

	attempts := retries + 1
	bestLoss := 100.0
	var lastErr error

	for i := 0; i < attempts; i++ {
		// запускаем: ping -I <iface> -c 1 -W <timeout> <target>
		cmd := exec.Command(
			"ping",
			"-I", interfaceName,
			"-c", "1",
			"-W", strconv.Itoa(timeoutSec),
			target,
		)
		out, err := cmd.CombinedOutput()
		text := string(out)

		if err != nil {
			lastErr = err
		}

		// разбираем packet loss
		loss := 100.0
		if m := lossRe.FindStringSubmatch(text); m != nil {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				loss = v
			}
		}

		// разбираем avg RTT
		avgRTT := 0.0
		if m := rttRe.FindStringSubmatch(text); m != nil {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				avgRTT = v
			}
		}

		// сохраняем лучший (минимальный) loss
		if loss < bestLoss {
			bestLoss = loss
			result.Loss = loss
			result.AvgTime = avgRTT
		}

		// если хоть что-то дошло — успех
		if loss < 100.0 {
			result.Success = true
			return result
		}

		// небольшая пауза между попытками
		time.Sleep(500 * time.Millisecond)
	}

	// ни один пакет не дошёл
	result.Success = false
	result.Error = fmt.Sprintf("all attempts failed: %v", lastErr)
	result.Loss = bestLoss
	return result
}

func testInterfacePing(iface NetworkInterface, targets []string, timeoutSec, retries int) []PingResult {
	// If interface is down or has no IP, return failures
	if iface.State != "UP" || iface.IP == "" {
		results := make([]PingResult, len(targets))
		for i, target := range targets {
			results[i] = PingResult{
				Target:    target,
				Interface: iface.Name,
				Success:   false,
				Error:     "Interface down or no IP",
			}
		}
		return results
	}

	// Sequential ping execution with direct slice population
	results := make([]PingResult, len(targets))
	for i, target := range targets {
		printDebugSafe(fmt.Sprintf("Pinging %s via %s...", target, iface.Name))
		pr := pingInterface(iface.Name, target, timeoutSec, retries)
		results[i] = pr
		if pr.Success {
			printDebugSafe(fmt.Sprintf("  %s via %s: %.1fms (%.0f%% loss)",
				pr.Target, iface.Name, pr.AvgTime, pr.Loss))
		} else {
			printDebugSafe(fmt.Sprintf("  %s via %s: %s",
				pr.Target, iface.Name, pr.Error))
		}
	}
	return results
}

func testRequirementPingParallel(matchingInterfaces []NetworkInterface, req NetworkRequirement, config *Config) map[string][]PingResult {
	results := make(map[string][]PingResult)

	// Filter valid interfaces for ping
	var testIfaces []NetworkInterface
	for _, iface := range matchingInterfaces {
		if iface.Type != "Loopback" && iface.Type != "Virtual" && iface.State == "UP" {
			testIfaces = append(testIfaces, iface)
		}
	}
	if len(testIfaces) == 0 {
		return results
	}

	retries := config.PingRetries
	if retries < 0 {
		retries = 0
	}

	// Channel for collecting interface results
	type ifaceResult struct {
		name    string
		results []PingResult
	}
	resultChan := make(chan ifaceResult, len(testIfaces))
	var wg sync.WaitGroup

	// Launch one goroutine per interface
	for _, iface := range testIfaces {
		wg.Add(1)
		go func(iface NetworkInterface) {
			defer wg.Done()
			pingResults := testInterfacePing(iface, req.PingTargets, req.PingTimeout, retries)
			resultChan <- ifaceResult{name: iface.Name, results: pingResults}
		}(iface)
	}

	// Wait and close channel
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Collect all
	for res := range resultChan {
		results[res.name] = res.results
	}

	return results
}

func checkNetworkAgainstRequirements(interfaces []NetworkInterface, config *Config) NetworkCheckResult {
	result := NetworkCheckResult{
		Status:   "ok",
		StateOK:  true,
		SpeedOK:  true,
		LinkOK:   true,
		PingOK:   true,
		DriverOK: true,
	}

	if len(config.NetworkRequirements) == 0 {
		return result
	}

	hasErrors := false
	hasWarnings := false

	for _, req := range config.NetworkRequirements {
		matchingInterfaces := filterInterfaces(interfaces, req)
		foundCount := len(matchingInterfaces)

		// Check interface count
		if req.MinInterfaces > 0 && foundCount < req.MinInterfaces {
			result.Issues = append(result.Issues,
				fmt.Sprintf("%s: found %d interface(s), required %d", req.Name, foundCount, req.MinInterfaces))
			hasErrors = true
		}

		if req.MaxInterfaces > 0 && foundCount > req.MaxInterfaces {
			result.Issues = append(result.Issues,
				fmt.Sprintf("%s: found %d interface(s), maximum %d", req.Name, foundCount, req.MaxInterfaces))
			hasErrors = true
		}

		// Check required interfaces
		for _, reqInterface := range req.RequiredInterfaces {
			found := false
			for _, iface := range interfaces {
				if iface.Name == reqInterface {
					found = true
					break
				}
			}
			if !found {
				result.Issues = append(result.Issues, fmt.Sprintf("Required interface %s not found", reqInterface))
				hasErrors = true
			}
		}

		// Check individual interface requirements
		var pingResultsMap map[string][]PingResult

		// If ping testing is enabled, do it in parallel for all matching interfaces
		if (config.CheckPing || req.CheckPing) && len(req.PingTargets) > 0 {
			printInfoSafe(fmt.Sprintf("Testing ping for %s interfaces in parallel", req.Name))
			pingResultsMap = testRequirementPingParallel(matchingInterfaces, req, config)
		}

		for _, iface := range matchingInterfaces {
			// Check expected state
			expectedState, hasExpectedState := req.ExpectedStates[iface.Name]
			if hasExpectedState {
				if iface.State != expectedState {
					if expectedState == "DOWN" && iface.State == "UP" {
						// Interface is UP but expected DOWN - warning, not error
						result.Issues = append(result.Issues,
							fmt.Sprintf("Interface %s: state %s (expected %s) - unexpected activation", iface.Name, iface.State, expectedState))
						result.StateWarn = true
						hasWarnings = true
					} else if expectedState == "UP" && iface.State == "DOWN" {
						// Interface is DOWN but expected UP - error
						result.Issues = append(result.Issues,
							fmt.Sprintf("Interface %s: state %s (expected %s)", iface.Name, iface.State, expectedState))
						result.StateOK = false
						hasErrors = true
					} else {
						// Other state mismatches
						result.Issues = append(result.Issues,
							fmt.Sprintf("Interface %s: state %s (expected %s)", iface.Name, iface.State, expectedState))
						result.StateOK = false
						hasErrors = true
					}
				}
			}

			// Skip further checks if interface is expected to be DOWN
			if hasExpectedState && expectedState == "DOWN" {
				if iface.State == "DOWN" {
					printDebugSafe(fmt.Sprintf("Interface %s is DOWN as expected - skipping additional checks", iface.Name))
				}
				continue // Skip link, speed, and ping checks for interfaces expected to be DOWN
			}

			// Check link status (only for interfaces expected to be UP or without specific expectation)
			if req.RequireLink && iface.Link == "down" {
				result.Issues = append(result.Issues,
					fmt.Sprintf("Interface %s: link down", iface.Name))
				result.LinkOK = false
				hasErrors = true
			}

			// Check speed (only for interfaces expected to be UP or without specific expectation)
			if req.RequiredSpeed != "" && iface.Speed != req.RequiredSpeed && iface.Speed != "unknown" {
				result.Issues = append(result.Issues,
					fmt.Sprintf("Interface %s: speed %s (required %s)", iface.Name, iface.Speed, req.RequiredSpeed))
				result.SpeedOK = false
				hasWarnings = true
			}

			// Process ping results if they were collected
			if pingResults, exists := pingResultsMap[iface.Name]; exists {
				failedPings := 0
				successfulPings := 0

				for _, pingResult := range pingResults {
					if pingResult.Success {
						successfulPings++
						printSuccessSafe(fmt.Sprintf("  %s via %s: %.1fms (%.0f%% loss)",
							pingResult.Target, iface.Name, pingResult.AvgTime, pingResult.Loss))
					} else {
						failedPings++
						printErrorSafe(fmt.Sprintf("  %s via %s: %s",
							pingResult.Target, iface.Name, pingResult.Error))
					}
				}

				if failedPings > 0 && successfulPings == 0 {
					// All pings failed
					result.Issues = append(result.Issues,
						fmt.Sprintf("Interface %s: all ping targets failed (%d/%d)",
							iface.Name, failedPings, len(req.PingTargets)))
					result.PingOK = false
					hasErrors = true
				} else if failedPings > 0 {
					// Some pings failed
					result.Issues = append(result.Issues,
						fmt.Sprintf("Interface %s: %d/%d ping targets failed",
							iface.Name, failedPings, len(req.PingTargets)))
					result.PingWarn = true
					hasWarnings = true
				} else {
					printSuccessSafe(fmt.Sprintf("Interface %s: all ping tests passed (%d/%d)",
						iface.Name, successfulPings, len(req.PingTargets)))
				}
			} else if (config.CheckPing || req.CheckPing) && len(req.PingTargets) > 0 &&
				iface.Type != "Loopback" && iface.Type != "Virtual" && iface.State == "DOWN" {
				printDebugSafe(fmt.Sprintf("Skipping ping test for interface %s - interface is DOWN", iface.Name))
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

func filterInterfaces(interfaces []NetworkInterface, req NetworkRequirement) []NetworkInterface {
	var matching []NetworkInterface

	for _, iface := range interfaces {
		// Check type
		if req.RequiredType != "" && req.RequiredType != "any" && iface.Type != req.RequiredType {
			continue
		}

		// Check specific required interfaces
		if len(req.RequiredInterfaces) > 0 {
			found := false
			for _, reqIface := range req.RequiredInterfaces {
				if iface.Name == reqIface {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		matching = append(matching, iface)
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

func formatSpeed(speed string) string {
	if speed == "" || speed == "unknown" {
		return "N/A"
	}
	return speed
}

func formatState(state string) string {
	switch state {
	case "UP":
		return "UP"
	case "DOWN":
		return "DOWN"
	default:
		return "?"
	}
}

func shortenInterfaceName(name string) string {
	if len(name) <= 6 {
		return name
	}

	// Try common abbreviations
	abbreviations := map[string]string{
		"ethernet": "eth",
		"enp":      "enp",
		"ens":      "ens",
		"eno":      "eno",
		"wlan":     "wlan",
		"docker":   "dock",
		"virbr":    "vbr",
	}

	for pattern, abbrev := range abbreviations {
		if strings.HasPrefix(strings.ToLower(name), pattern) {
			return abbrev + name[len(pattern):]
		}
	}

	// Fallback: take first 6 characters
	return name[:6]
}

func visualizeSlots(interfaces []NetworkInterface, config *Config) error {
	printInfo("Network Interfaces Layout:")
	fmt.Println()

	maxSlots := config.Visualization.TotalSlots
	if maxSlots == 0 {
		maxSlots = len(interfaces)
	}

	// Collect required interfaces
	required := make(map[string]bool)
	for _, req := range config.NetworkRequirements {
		for _, iface := range req.RequiredInterfaces {
			required[iface] = true
		}
	}

	// Create position to interface mapping
	posToInterface := make(map[int]string, len(config.Visualization.InterfaceMapping))
	for ifaceName, pos := range config.Visualization.InterfaceMapping {
		posToInterface[pos] = ifaceName
	}

	// Fill slot data array
	slotData := make([]NetworkInterface, maxSlots+1)
	for _, iface := range interfaces {
		if pos, ok := config.Visualization.InterfaceMapping[iface.Name]; ok && pos >= 1 && pos <= maxSlots {
			slotData[pos] = iface
		}
	}

	// System check for coloring
	systemResult := checkNetworkAgainstRequirements(interfaces, config)

	// Legend
	printInfo("Legend:")
	fmt.Printf("  %s%s%s Interface UP     ", ColorGreen, "▓▓▓", ColorReset)
	fmt.Printf("  %s%s%s DOWN (Expected)  ", ColorGray, "▓▓▓", ColorReset)
	fmt.Printf("  %s%s%s DOWN (Unexpected)", ColorYellow, "▓▓▓", ColorReset)
	fmt.Printf("  %sMISS%s Missing Required", ColorRed, ColorReset)
	fmt.Printf("  %s%s%s Empty Slot\n", ColorWhite, "░░░", ColorReset)
	fmt.Println()

	// Generate rows
	var rows []RowConfig
	if config.Visualization.CustomRows.Enabled && len(config.Visualization.CustomRows.Rows) > 0 {
		rows = config.Visualization.CustomRows.Rows
	} else {
		perRow := config.Visualization.InterfacesPerRow
		if perRow == 0 {
			perRow = 6
		}
		for start := 1; start <= maxSlots; start += perRow {
			end := start + perRow - 1
			if end > maxSlots {
				end = maxSlots
			}
			rows = append(rows, RowConfig{
				Name:       fmt.Sprintf("Network Bank %d", len(rows)+1),
				Interfaces: fmt.Sprintf("%d-%d", start, end),
			})
		}
	}

	// Visualize each row
	for _, row := range rows {
		// Parse interface positions for this row
		var positions []int
		if strings.Contains(row.Interfaces, "-") {
			parts := strings.Split(row.Interfaces, "-")
			if len(parts) == 2 {
				start, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
				end, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
				for i := start; i <= end && i <= maxSlots; i++ {
					positions = append(positions, i)
				}
			}
		} else {
			// Single position or comma-separated list
			posStrs := strings.Split(row.Interfaces, ",")
			for _, posStr := range posStrs {
				if pos, err := strconv.Atoi(strings.TrimSpace(posStr)); err == nil && pos >= 1 && pos <= maxSlots {
					positions = append(positions, pos)
				}
			}
		}

		if len(positions) == 0 {
			printWarning(fmt.Sprintf("Skipping invalid row '%s'", row.Interfaces))
			continue
		}

		count := len(positions)
		width := config.Visualization.SlotWidth
		if width < 4 {
			width = 4
		}

		// Row header
		if len(rows) > 1 {
			fmt.Printf("%s (Interfaces %s):\n", row.Name, row.Interfaces)
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
			pos := positions[i]
			iface := slotData[pos]

			if iface.IsPresent {
				vis := getNetworkVisual(iface, &config.Visualization)
				sym := centerText(vis.Symbol, width)
				color := getANSIColor(vis.Color)

				// Check if interface state matches expectations
				expectedDown := false
				for _, req := range config.NetworkRequirements {
					if expectedState, exists := req.ExpectedStates[iface.Name]; exists && expectedState == "DOWN" {
						expectedDown = true
						break
					}
				}

				if iface.State == "DOWN" && expectedDown {
					// Interface is DOWN as expected - show in neutral/gray color
					fmt.Print(ColorGray + sym + ColorReset)
				} else if iface.State == "DOWN" {
					// Interface is DOWN but not expected - show in yellow (warning)
					fmt.Print(ColorYellow + sym + ColorReset)
				} else {
					// Interface is UP - show in its normal color
					fmt.Print(color + sym + ColorReset)
				}
			} else {
				ifaceName := posToInterface[pos]
				if required[ifaceName] {
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
			pos := positions[i]
			iface := slotData[pos]
			if iface.IsPresent {
				vis := getNetworkVisual(iface, &config.Visualization)
				txt := centerText(vis.ShortName, width)

				// Check if interface state matches expectations
				expectedDown := false
				for _, req := range config.NetworkRequirements {
					if expectedState, exists := req.ExpectedStates[iface.Name]; exists && expectedState == "DOWN" {
						expectedDown = true
						break
					}
				}

				if iface.State == "DOWN" && expectedDown {
					// Interface is DOWN as expected - show in neutral/gray color
					fmt.Print(ColorGray + txt + ColorReset)
				} else if iface.State == "DOWN" {
					// Interface is DOWN but not expected - show in yellow (warning)
					fmt.Print(ColorYellow + txt + ColorReset)
				} else {
					// Interface is UP - show in green
					fmt.Print(ColorGreen + txt + ColorReset)
				}
			} else {
				fmt.Print(strings.Repeat(" ", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// State row
		fmt.Print("│")
		for i := 0; i < count; i++ {
			pos := positions[i]
			iface := slotData[pos]
			if iface.IsPresent {
				txt := centerText(formatState(iface.State), width)

				// Check if interface state matches expectations
				expectedDown := false
				for _, req := range config.NetworkRequirements {
					if expectedState, exists := req.ExpectedStates[iface.Name]; exists && expectedState == "DOWN" {
						expectedDown = true
						break
					}
				}

				if iface.State == "DOWN" && expectedDown {
					// Interface is DOWN as expected - show in neutral/gray color
					fmt.Print(ColorGray + txt + ColorReset)
				} else if iface.State == "DOWN" {
					// Interface is DOWN but not expected - show in yellow (warning)
					fmt.Print(ColorYellow + txt + ColorReset)
				} else if iface.State == "UP" {
					// Interface is UP - show in green
					fmt.Print(ColorGreen + txt + ColorReset)
				} else {
					// Unknown state
					fmt.Print(ColorWhite + txt + ColorReset)
				}
			} else {
				fmt.Print(strings.Repeat(" ", width))
			}
			fmt.Print("│")
		}
		fmt.Println()

		// Speed row (if enabled)
		if config.CheckSpeed {
			fmt.Print("│")
			for i := 0; i < count; i++ {
				pos := positions[i]
				iface := slotData[pos]
				if iface.IsPresent {
					txt := centerText(formatSpeed(iface.Speed), width)

					// Check if interface state matches expectations
					expectedDown := false
					for _, req := range config.NetworkRequirements {
						if expectedState, exists := req.ExpectedStates[iface.Name]; exists && expectedState == "DOWN" {
							expectedDown = true
							break
						}
					}

					if iface.State == "DOWN" && expectedDown {
						// Interface is DOWN as expected - show in neutral/gray color
						fmt.Print(ColorGray + txt + ColorReset)
					} else if iface.State == "DOWN" {
						// Interface is DOWN but not expected - show in yellow (warning)
						fmt.Print(ColorYellow + txt + ColorReset)
					} else {
						// Interface is UP - show in green
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

		// Position numbers
		fmt.Print(" ")
		for i := 0; i < count; i++ {
			fmt.Print(centerText(fmt.Sprintf("%d", positions[i]), width+1))
		}
		fmt.Println(" (Pos)")

		// Interface names
		fmt.Print(" ")
		for i := 0; i < count; i++ {
			pos := positions[i]
			name := posToInterface[pos]
			short := shortenInterfaceName(name)
			fmt.Print(centerText(short, width+1))
		}
		fmt.Printf(" (Name)\n\n")
	}

	// Final status
	switch systemResult.Status {
	case "error":
		printError("Network configuration validation FAILED!")
		return fmt.Errorf("validation failed")
	case "warning":
		printWarning("Validation completed with warnings")
	default:
		printSuccess("All network interfaces meet requirements!")
	}
	return nil
}

func getNetworkVisual(iface NetworkInterface, config *VisualizationConfig) NetworkVisual {
	if !iface.IsPresent {
		return NetworkVisual{
			Symbol:      "░░░",
			ShortName:   "",
			Description: "Empty Slot",
			Color:       "gray",
		}
	}

	ifaceType := iface.Type
	if ifaceType == "" {
		ifaceType = "Other"
	}

	if visual, exists := config.TypeVisuals[ifaceType]; exists {
		return visual
	}

	// Fallback
	return NetworkVisual{
		Symbol:      "???",
		ShortName:   ifaceType,
		Description: ifaceType + " Interface",
		Color:       "white",
	}
}

func testAllInterfacesPing(interfaces []NetworkInterface, config *Config) error {
	printInfo("Starting parallel ping tests for all interfaces...")

	if !config.CheckPing {
		printWarning("Ping testing is disabled in configuration")
		return nil
	}

	// Find ping targets from requirements
	var targets []string
	for _, req := range config.NetworkRequirements {
		if req.CheckPing && len(req.PingTargets) > 0 {
			targets = req.PingTargets
			break
		}
	}

	if len(targets) == 0 {
		targets = []string{"8.8.8.8", "1.1.1.1"} // Default targets
	}

	timeout := config.PingTimeout
	if timeout == 0 {
		timeout = 5
	}

	printInfo(fmt.Sprintf("Ping targets: %s", strings.Join(targets, ", ")))
	printInfo(fmt.Sprintf("Timeout: %d seconds", timeout))

	// Filter interfaces for testing
	var testInterfaces []NetworkInterface
	for _, iface := range interfaces {
		if iface.Type == "Loopback" || iface.Type == "Virtual" {
			continue // Skip loopback and virtual interfaces
		}

		// Check if interface is expected to be DOWN
		expectedDown := false
		for _, req := range config.NetworkRequirements {
			if expectedState, exists := req.ExpectedStates[iface.Name]; exists && expectedState == "DOWN" {
				expectedDown = true
				break
			}
		}

		if expectedDown {
			printInfoSafe(fmt.Sprintf("Interface %s is expected to be DOWN - skipping ping test", iface.Name))
			continue
		}

		if iface.State != "UP" {
			printWarningSafe(fmt.Sprintf("Interface %s is %s - skipping ping test", iface.Name, iface.State))
			continue
		}

		if iface.IP == "" {
			printWarningSafe(fmt.Sprintf("Interface %s has no IP address - skipping ping test", iface.Name))
			continue
		}

		testInterfaces = append(testInterfaces, iface)
	}

	if len(testInterfaces) == 0 {
		printWarning("No interfaces available for ping testing")
		return nil
	}

	// Parallel execution for all interfaces
	type interfaceResult struct {
		Interface    NetworkInterface
		SuccessCount int
		TotalCount   int
		Results      []PingResult
	}

	resultChan := make(chan interfaceResult, len(testInterfaces))
	var wg sync.WaitGroup

	// Start ping tests for all interfaces in parallel
	for _, iface := range testInterfaces {
		wg.Add(4)
		go func(testIface NetworkInterface) {
			defer wg.Done()

			printInfoSafe(fmt.Sprintf("Testing interface %s (%s) - IP: %s", testIface.Name, testIface.Type, testIface.IP))

			retries := config.PingRetries
			if retries < 0 {
				retries = 0
			}

			results := testInterfacePing(testIface, targets, timeout, retries)
			successCount := 0
			for _, result := range results {
				if result.Success {
					successCount++
				}
			}

			resultChan <- interfaceResult{
				Interface:    testIface,
				SuccessCount: successCount,
				TotalCount:   len(targets),
				Results:      results,
			}
		}(iface)
	}

	// Wait for all tests to complete
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Collect and display results
	allSuccess := true
	var results []interfaceResult
	for result := range resultChan {
		results = append(results, result)
	}

	// Display results in order
	printInfo("\nPing test results summary:")
	for _, result := range results {
		iface := result.Interface
		successCount := result.SuccessCount
		totalCount := result.TotalCount

		if successCount == totalCount {
			printSuccessSafe(fmt.Sprintf("Interface %s: All ping tests passed (%d/%d)", iface.Name, successCount, totalCount))
		} else if successCount > 0 {
			printWarningSafe(fmt.Sprintf("Interface %s: Partial ping success (%d/%d)", iface.Name, successCount, totalCount))
			allSuccess = false
		} else {
			printErrorSafe(fmt.Sprintf("Interface %s: All ping tests failed (%d/%d)", iface.Name, successCount, totalCount))
			allSuccess = false
		}

		// Show detailed results for each target
		for _, pingResult := range result.Results {
			if pingResult.Success {
				printSuccessSafe(fmt.Sprintf("  %s via %s: %.1fms (%.0f%% loss)",
					pingResult.Target, iface.Name, pingResult.AvgTime, pingResult.Loss))
			} else {
				printErrorSafe(fmt.Sprintf("  %s via %s: %s",
					pingResult.Target, iface.Name, pingResult.Error))
			}
		}
	}

	if allSuccess {
		printSuccess("\nAll interface ping tests completed successfully")
	} else {
		printWarning("\nSome interface ping tests failed")
		return fmt.Errorf("ping tests failed for some interfaces")
	}

	return nil
}

func checkNetwork(config *Config) error {
	printInfo("Starting network interface check...")

	interfaces, err := getNetworkInterfaces()
	if err != nil {
		return fmt.Errorf("failed to get network interface info: %v", err)
	}

	printInfo(fmt.Sprintf("Found network interfaces: %d", len(interfaces)))

	if len(interfaces) == 0 {
		printError("No network interfaces found")
		return fmt.Errorf("no network interfaces found")
	}

	// Display found interfaces
	for i, iface := range interfaces {
		if iface.Type == "Loopback" {
			printInfo(fmt.Sprintf("Interface %d: %s (%s)", i+1, iface.Name, iface.Type))
		} else {
			printInfo(fmt.Sprintf("Interface %d: %s (%s) - %s", i+1, iface.Name, iface.Type, iface.State))
			if iface.IP != "" {
				printDebug(fmt.Sprintf("  IP: %s", iface.IP))
			}
			if iface.MAC != "" {
				printDebug(fmt.Sprintf("  MAC: %s", iface.MAC))
			}
			if iface.Driver != "" {
				printDebug(fmt.Sprintf("  Driver: %s", iface.Driver))
			}
			if iface.Speed != "" && iface.Speed != "unknown" {
				printDebug(fmt.Sprintf("  Speed: %s", iface.Speed))
			}
		}
	}

	// Check requirements (this now includes ping testing)
	result := checkNetworkAgainstRequirements(interfaces, config)

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
		printError("Network interface requirements FAILED")
		return fmt.Errorf("network interface requirements not met")
	} else if result.Status == "warning" {
		printWarning("Network interface requirements passed with warnings")
	} else {
		printSuccess("All network interface requirements passed")
	}

	return nil
}

func main() {
	var (
		showVersion  = flag.Bool("V", false, "Show version")
		configPath   = flag.String("c", "network_config.json", "Path to configuration file")
		createConfig = flag.Bool("s", false, "Create default configuration file")
		showHelpFlag = flag.Bool("h", false, "Show help")
		listOnly     = flag.Bool("l", false, "List detected network interfaces without configuration check")
		visualize    = flag.Bool("vis", false, "Show visual network interfaces layout")
		testPing     = flag.Bool("ping", false, "Test ping on all interfaces (requires configuration)")
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
		printInfo("Scanning for network interfaces...")
		interfaces, err := getNetworkInterfaces()
		if err != nil {
			printError(fmt.Sprintf("Error getting network interface information: %v", err))
			os.Exit(1)
		}

		if len(interfaces) == 0 {
			printWarning("No network interfaces found")
		} else {
			printSuccess(fmt.Sprintf("Found network interfaces: %d", len(interfaces)))

			for i, iface := range interfaces {
				fmt.Printf("\nInterface %d:\n", i+1)
				fmt.Printf("  Name: %s\n", iface.Name)
				fmt.Printf("  Type: %s\n", iface.Type)
				fmt.Printf("  State: %s\n", iface.State)
				fmt.Printf("  MAC: %s\n", iface.MAC)
				fmt.Printf("  IP: %s\n", iface.IP)
				fmt.Printf("  Driver: %s\n", iface.Driver)
				fmt.Printf("  PCI Slot: %s\n", iface.PCISlot)
				fmt.Printf("  Link: %s\n", iface.Link)
				fmt.Printf("  Speed: %s\n", formatSpeed(iface.Speed))
			}
		}
		return
	}

	if *testPing {
		config, err := loadConfig(*configPath)
		if err != nil {
			printError(fmt.Sprintf("Error loading configuration: %v", err))
			printInfo("Use -s to create a default configuration file")
			os.Exit(1)
		}

		interfaces, err := getNetworkInterfaces()
		if err != nil {
			printError(fmt.Sprintf("Error getting network interfaces: %v", err))
			os.Exit(1)
		}

		err = testAllInterfacesPing(interfaces, config)
		if err != nil {
			os.Exit(1)
		}
		return
	}

	if *visualize {
		printInfo("Scanning for network interfaces...")
		interfaces, err := getNetworkInterfaces()
		if err != nil {
			printError(fmt.Sprintf("Error getting network interface information: %v", err))
			os.Exit(1)
		}

		config, err := loadConfig(*configPath)
		if err != nil {
			printError(fmt.Sprintf("Error loading configuration: %v", err))
			printInfo("Use -s to create a default configuration file")
			os.Exit(1)
		}

		err = visualizeSlots(interfaces, config)
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
		printInfo("Or use -l to simply display found network interfaces")
		os.Exit(1)
	}

	printInfo(fmt.Sprintf("Configuration loaded from: %s", *configPath))

	err = checkNetwork(config)
	if err != nil {
		printError(fmt.Sprintf("Network interface check failed: %v", err))
		os.Exit(1)
	}
}
