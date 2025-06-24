package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const VERSION = "2.0.0"

// ANSI color codes
const (
	ColorReset  = "\033[0m"
	ColorGreen  = "\033[92m"
	ColorBlue   = "\033[34m"
	ColorWhite  = "\033[37m"
	ColorYellow = "\033[33m"
	ColorRed    = "\033[31m"
)

// Configuration structures
type Config struct {
	System SystemConfig `yaml:"system"`
	Tests  TestsConfig  `yaml:"tests"`
	Flash  FlashConfig  `yaml:"flash,omitempty"`
	Log    LogConfig    `yaml:"log"`
}

type SystemConfig struct {
	Product string `yaml:"product"`
}

type TestsConfig struct {
	ParallelGroups   [][]TestSpec `yaml:"parallel_groups,omitempty"`
	SequentialGroups [][]TestSpec `yaml:"sequential_groups,omitempty"`
}

type TestSpec struct {
	Name    string   `yaml:"name"`
	Command string   `yaml:"command"`
	Args    []string `yaml:"args,omitempty"`
	Type    string   `yaml:"type"` // "standard" or "tui"
	Timeout string   `yaml:"timeout,omitempty"`
}

type FlashConfig struct {
	Enabled    bool     `yaml:"enabled"`
	Operations []string `yaml:"operations,omitempty"`
}

type LogConfig struct {
	SaveLocal bool   `yaml:"save_local"`
	Server    string `yaml:"server,omitempty"`
	LogDir    string `yaml:"log_dir,omitempty"`
}

type FlashData struct {
	MBSerial string
	IOSerial string
	MAC      string
}

// Result structures
type TestResult struct {
	Name     string        `yaml:"name"`
	Status   string        `yaml:"status"` // "PASSED", "FAILED", "TIMEOUT"
	Duration time.Duration `yaml:"duration"`
	Error    string        `yaml:"error,omitempty"`
	Output   string        `yaml:"-"` // Not saved to log
}

type SystemInfo struct {
	Product   string                 `yaml:"product"`
	MBSerial  string                 `yaml:"mb_serial"`
	IOSerial  string                 `yaml:"io_serial,omitempty"`
	MAC       string                 `yaml:"mac,omitempty"`
	IP        string                 `yaml:"ip,omitempty"`
	Timestamp time.Time              `yaml:"timestamp"`
	DMIDecode map[string]interface{} `yaml:"dmidecode"`
}

type SessionLog struct {
	SessionID    string        `yaml:"session"`
	Timestamp    time.Time     `yaml:"timestamp"`
	System       SystemInfo    `yaml:"system"`
	Pipeline     PipelineInfo  `yaml:"pipeline"`
	TestResults  []TestResult  `yaml:"test_results"`
	FlashResults []FlashResult `yaml:"flash_results,omitempty"`
}

type PipelineInfo struct {
	Mode     string        `yaml:"mode"`
	Config   string        `yaml:"config"`
	Duration time.Duration `yaml:"duration"`
}

type FlashResult struct {
	Operation string        `yaml:"operation"`
	Status    string        `yaml:"status"`
	Duration  time.Duration `yaml:"duration"`
	Details   string        `yaml:"details,omitempty"`
}

// Output manager for synchronized output
type OutputManager struct {
	mutex sync.Mutex
}

func (om *OutputManager) PrintSection(title, content string) {
	om.mutex.Lock()
	defer om.mutex.Unlock()

	fmt.Printf("\n--- %s ---\n", title)
	fmt.Print(content)
	if !strings.HasSuffix(content, "\n") {
		fmt.Println()
	}
}

func (om *OutputManager) PrintResult(timestamp time.Time, name, status string, duration time.Duration, err string) {
	om.mutex.Lock()
	defer om.mutex.Unlock()

	icon := "✓"
	color := ColorGreen
	if status == "FAILED" {
		icon = "❌"
		color = ColorRed
	} else if status == "TIMEOUT" {
		icon = "⏰"
		color = ColorYellow
	}

	fmt.Printf("[%s] %s%s %s %s (%s)%s\n",
		timestamp.Format("15:04:05"),
		color, icon, name, status, duration.Round(100*time.Millisecond), ColorReset)

	if err != "" {
		fmt.Printf("%sERROR: %s%s\n", ColorRed, err, ColorReset)
	}
}

var outputManager = &OutputManager{}

func printColored(color, message string) {
	fmt.Printf("%s%s%s\n", color, message, ColorReset)
}

func printInfo(message string) {
	printColored(ColorBlue, message)
}

func printSuccess(message string) {
	printColored(ColorGreen, message)
}

func printError(message string) {
	printColored(ColorRed, message)
}

func showHelp() {
	fmt.Printf("System Validator %s\n", VERSION)
	fmt.Println("Parameters:")
	fmt.Println("  -V          Show program version")
	fmt.Println("  -c <path>   Path to configuration file (default: config.yaml)")
	fmt.Println("  -tests-only Run only tests (skip flashing)")
	fmt.Println("  -flash-only Run only flashing (skip tests)")
	fmt.Println("  -h          Show this help")
}

func loadConfig(configPath string) (*Config, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}

	return &config, nil
}

func runTest(test TestSpec, outputMgr *OutputManager) TestResult {
	result := TestResult{
		Name:   test.Name,
		Status: "FAILED",
	}

	startTime := time.Now()

	// Parse timeout
	timeout := 30 * time.Second
	if test.Timeout != "" {
		if t, err := time.ParseDuration(test.Timeout); err == nil {
			timeout = t
		}
	}

	// Create command
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, test.Command, test.Args...)

	// Capture both stdout and stderr
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run command
	err := cmd.Run()
	result.Duration = time.Since(startTime)

	// Combine output for display
	output := stdout.String() + stderr.String()
	result.Output = output

	// Determine result
	if ctx.Err() == context.DeadlineExceeded {
		result.Status = "TIMEOUT"
		result.Error = fmt.Sprintf("Test timed out after %s", timeout)
	} else if err != nil {
		result.Status = "FAILED"
		// Try to get error message from stderr
		if stderr.Len() > 0 {
			lines := strings.Split(stderr.String(), "\n")
			for _, line := range lines {
				if strings.HasPrefix(line, "ERROR:") {
					result.Error = strings.TrimPrefix(line, "ERROR:")
					result.Error = strings.TrimSpace(result.Error)
					break
				}
			}
		}
		if result.Error == "" {
			result.Error = fmt.Sprintf("Exit code: %d", cmd.ProcessState.ExitCode())
		}
	} else {
		result.Status = "PASSED"
	}

	// Print output if test produced any
	if output != "" {
		outputMgr.PrintSection(test.Name+" Output", output)
	}

	// Print result
	outputMgr.PrintResult(time.Now(), test.Name, result.Status, result.Duration, result.Error)

	return result
}

func runTestGroup(tests []TestSpec, parallel bool, outputMgr *OutputManager, groupName string) []TestResult {
	printInfo(fmt.Sprintf("==> %s", groupName))

	if parallel {
		// Run tests in parallel
		var wg sync.WaitGroup
		results := make([]TestResult, len(tests))

		for _, test := range tests {
			fmt.Printf("[%s] %s........................ RUNNING\n",
				time.Now().Format("15:04:05"), test.Name)
		}

		for i, test := range tests {
			wg.Add(1)
			go func(idx int, t TestSpec) {
				defer wg.Done()
				results[idx] = runTest(t, outputMgr)
			}(i, test)
		}

		wg.Wait()
		return results
	} else {
		// Run tests sequentially
		var results []TestResult
		for _, test := range tests {
			fmt.Printf("[%s] %s........................ RUNNING\n",
				time.Now().Format("15:04:05"), test.Name)
			result := runTest(test, outputMgr)
			results = append(results, result)
		}
		return results
	}
}

func getFlashData(productName string) (*FlashData, error) {
	if productName == "" {
		return nil, fmt.Errorf("product name not detected")
	}

	printInfo(fmt.Sprintf("Product Name: %s", productName))

	// Define required fields based on product type
	requiredFields := make(map[string]*regexp.Regexp)

	switch productName {
	case "Silver":
		requiredFields["mbSN"] = regexp.MustCompile(`^INF0[0-9]{1}A3[0-9]{8}$`)
		requiredFields["ioSN"] = regexp.MustCompile(`^INF0[0-9]{1}A4[0-9]{8}$`)
		requiredFields["mac"] = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)
	case "IFMBH610MTPR":
		requiredFields["mbSN"] = regexp.MustCompile(`^INF0[0-9]{1}A9[0-9]{8}$`)
		requiredFields["mac"] = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)
	case "IFMBB760M":
		requiredFields["mbSN"] = regexp.MustCompile(`^INF0[0-9]{1}B4[0-9]{8}$`)
		requiredFields["mac"] = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)
	case "Mercury":
		requiredFields["mbSN"] = regexp.MustCompile(`^INF0[0-9]{1}B7[0-9]{8}$`)
		requiredFields["ioSN"] = regexp.MustCompile(`^INF0[0-9]{1}B8[0-9]{8}$`)
		requiredFields["mac"] = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)
	default:
		return nil, fmt.Errorf("unknown product name: %s", productName)
	}

	fmt.Println("Please enter the following values (the program will automatically detect the type):")
	for key, regex := range requiredFields {
		fmt.Printf(" - %s (expected format: %s)\n", key, regex.String())
	}

	provided := make(map[string]string)
	reader := bufio.NewReader(os.Stdin)

	for len(provided) < len(requiredFields) {
		fmt.Print("Enter value: ")
		input, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		input = strings.TrimSpace(input)
		if input == "" {
			fmt.Println("Input cannot be empty. Please re-enter.")
			continue
		}

		matched := false
		for key, regex := range requiredFields {
			if _, ok := provided[key]; ok {
				continue
			}
			if regex.MatchString(input) {
				provided[key] = input
				fmt.Printf("%s value accepted: %s\n", key, input)
				matched = true
				break
			}
		}
		if !matched {
			fmt.Println("Input does not match any expected format. Please try again.")
		}
	}

	flashData := &FlashData{}
	if val, ok := provided["mbSN"]; ok {
		flashData.MBSerial = val
	}
	if val, ok := provided["ioSN"]; ok {
		flashData.IOSerial = val
	}
	if val, ok := provided["mac"]; ok {
		flashData.MAC = val
	}

	fmt.Println("Collected data:")
	fmt.Printf("  MB Serial: %s\n", flashData.MBSerial)
	if flashData.IOSerial != "" {
		fmt.Printf("  IO Serial: %s\n", flashData.IOSerial)
	}
	fmt.Printf("  MAC: %s\n", flashData.MAC)

	return flashData, nil
}

func getSystemInfo() (SystemInfo, error) {
	info := SystemInfo{
		Timestamp: time.Now(),
	}

	// Get IP address
	if ip, err := getIPAddress(); err == nil {
		info.IP = ip
	}

	// Run dmidecode
	cmd := exec.Command("dmidecode")
	output, err := cmd.Output()
	if err != nil {
		return info, fmt.Errorf("failed to run dmidecode: %v", err)
	}

	// Parse dmidecode output
	dmidecodeData := parseDMIDecode(string(output))
	info.DMIDecode = dmidecodeData

	// Extract key information
	if systemInfo, ok := dmidecodeData["System Information"].(map[string]interface{}); ok {
		if product, ok := systemInfo["Product Name"].(string); ok {
			info.Product = product
		}
	}

	if baseboardInfo, ok := dmidecodeData["Base Board Information"].(map[string]interface{}); ok {
		if serial, ok := baseboardInfo["Serial Number"].(string); ok {
			info.MBSerial = serial
		}
	}

	return info, nil
}

func getIPAddress() (string, error) {
	cmd := exec.Command("hostname", "-I")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	ips := strings.Fields(string(output))
	if len(ips) > 0 {
		return ips[0], nil
	}

	return "", fmt.Errorf("no IP address found")
}

func parseDMIDecode(output string) map[string]interface{} {
	result := make(map[string]interface{})

	lines := strings.Split(output, "\n")
	var currentSection string
	var currentData map[string]interface{}

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if line == "" {
			continue
		}

		// Check if this is a section header
		if !strings.HasPrefix(line, "\t") && strings.Contains(line, "Information") {
			if currentSection != "" && currentData != nil {
				result[currentSection] = currentData
			}
			currentSection = line
			currentData = make(map[string]interface{})
			continue
		}

		// Parse key-value pairs
		if strings.Contains(line, ":") && currentData != nil {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])
				currentData[key] = value
			}
		}
	}

	// Add the last section
	if currentSection != "" && currentData != nil {
		result[currentSection] = currentData
	}

	return result
}

func runFlashing(config FlashConfig, flashData *FlashData) []FlashResult {
	var results []FlashResult

	if !config.Enabled {
		return results
	}

	printInfo("FLASHING PHASE [2/2]")
	printInfo("===================")

	for _, operation := range config.Operations {
		result := FlashResult{
			Operation: operation,
			Status:    "PASSED",
		}

		startTime := time.Now()

		// Simulate flashing operations
		switch operation {
		case "serial_number":
			printInfo(fmt.Sprintf("Flashing serial number: %s", flashData.MBSerial))
			time.Sleep(2 * time.Second) // Simulate work
			result.Details = fmt.Sprintf("Updated MB serial to %s", flashData.MBSerial)

		case "mac_address":
			printInfo(fmt.Sprintf("Flashing MAC address: %s", flashData.MAC))
			time.Sleep(1 * time.Second) // Simulate work
			result.Details = fmt.Sprintf("Updated MAC to %s", flashData.MAC)

		case "efi_variables":
			printInfo("Updating EFI variables")
			time.Sleep(1 * time.Second) // Simulate work
			result.Details = "EFI variables updated"
		}

		result.Duration = time.Since(startTime)
		results = append(results, result)

		outputManager.PrintResult(time.Now(), operation, result.Status, result.Duration, "")
	}

	return results
}

func saveLog(log SessionLog, config LogConfig) error {
	if !config.SaveLocal {
		return nil
	}

	logDir := config.LogDir
	if logDir == "" {
		logDir = "logs"
	}

	// Create log directory
	err := os.MkdirAll(logDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create log directory: %v", err)
	}

	// Generate filename
	timestamp := log.Timestamp.Format("20060102_150405")
	filename := fmt.Sprintf("%s_%s_%s.yaml", log.System.Product, log.System.MBSerial, timestamp)
	filepath := filepath.Join(logDir, filename)

	// Marshal to YAML
	data, err := yaml.Marshal(log)
	if err != nil {
		return fmt.Errorf("failed to marshal log: %v", err)
	}

	// Write to file
	err = os.WriteFile(filepath, data, 0644)
	if err != nil {
		return fmt.Errorf("failed to write log file: %v", err)
	}

	printSuccess(fmt.Sprintf("Log saved: %s", filepath))
	return nil
}

func sendLogToServer(log SessionLog, server string) error {
	if server == "" {
		return nil
	}

	printInfo(fmt.Sprintf("Sending log to server: %s", server))

	// Marshal to YAML
	data, err := yaml.Marshal(log)
	if err != nil {
		return fmt.Errorf("failed to marshal log: %v", err)
	}

	// Create temporary file
	tmpFile, err := os.CreateTemp("", "system_validator_*.yaml")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.Write(data)
	if err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write temp file: %v", err)
	}
	tmpFile.Close()

	// Generate remote filename
	timestamp := log.Timestamp.Format("20060102_150405")
	remoteFile := fmt.Sprintf("%s_%s_%s.yaml", log.System.Product, log.System.MBSerial, timestamp)

	// Use SCP to send file
	cmd := exec.Command("scp",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		tmpFile.Name(),
		fmt.Sprintf("%s/%s", server, remoteFile))

	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("failed to send log via SCP: %v", err)
	}

	printSuccess("Log sent to server successfully")
	return nil
}

func main() {
	// Parse command line arguments
	args := os.Args[1:]

	var configPath = "config.yaml"
	var testsOnly, flashOnly bool

	for i, arg := range args {
		switch arg {
		case "-V":
			fmt.Println(VERSION)
			return
		case "-h":
			showHelp()
			return
		case "-c":
			if i+1 < len(args) {
				configPath = args[i+1]
			}
		case "-tests-only":
			testsOnly = true
		case "-flash-only":
			flashOnly = true
		}
	}

	// Check root privileges
	if os.Geteuid() != 0 {
		printError("This program requires root privileges")
		os.Exit(1)
	}

	// Load configuration
	config, err := loadConfig(configPath)
	if err != nil {
		printError(fmt.Sprintf("Failed to load configuration: %v", err))
		os.Exit(1)
	}

	// Print header
	fmt.Printf("SYSTEM VALIDATOR v%s | %s | %s\n", VERSION, config.System.Product, configPath)
	fmt.Println(strings.Repeat("=", 80))

	startTime := time.Now()

	// Get system information
	systemInfo, err := getSystemInfo()
	if err != nil {
		printError(fmt.Sprintf("Failed to get system information: %v", err))
		os.Exit(1)
	}

	printInfo(fmt.Sprintf("System detected: %s, Serial: %s, IP: %s",
		systemInfo.Product, systemInfo.MBSerial, systemInfo.IP))

	var allResults []TestResult
	var flashResults []FlashResult
	var flashData *FlashData

	// Run tests
	if !flashOnly {
		printInfo("TESTING PHASE [1/2]")
		printInfo("===================")

		// Run parallel groups
		for i, group := range config.Tests.ParallelGroups {
			groupName := fmt.Sprintf("Parallel Group %d", i+1)
			results := runTestGroup(group, true, outputManager, groupName)
			allResults = append(allResults, results...)
		}

		// Run sequential groups
		for i, group := range config.Tests.SequentialGroups {
			groupName := fmt.Sprintf("Sequential Group %d", i+1)
			results := runTestGroup(group, false, outputManager, groupName)
			allResults = append(allResults, results...)
		}
	}

	// Get flash data from user input (if flashing is enabled)
	if !testsOnly && config.Flash.Enabled {
		fmt.Println()
		printInfo("PREPARATION FOR FLASHING")
		printInfo("========================")

		var err error
		flashData, err = getFlashData(systemInfo.Product)
		if err != nil {
			printError(fmt.Sprintf("Failed to get flash data: %v", err))
			os.Exit(1)
		}
	}

	// Run flashing
	if !testsOnly && config.Flash.Enabled && flashData != nil {
		flashResults = runFlashing(config.Flash, flashData)
	}

	totalDuration := time.Since(startTime)

	// Create session log
	sessionLog := SessionLog{
		SessionID: fmt.Sprintf("%d", time.Now().Unix()),
		Timestamp: startTime,
		System:    systemInfo,
		Pipeline: PipelineInfo{
			Mode:     "full",
			Config:   configPath,
			Duration: totalDuration,
		},
		TestResults:  allResults,
		FlashResults: flashResults,
	}

	// Add flash data to system info if available
	if flashData != nil {
		sessionLog.System.MBSerial = flashData.MBSerial
		sessionLog.System.IOSerial = flashData.IOSerial
		sessionLog.System.MAC = flashData.MAC
	}

	// Save and send logs
	if err := saveLog(sessionLog, config.Log); err != nil {
		printError(fmt.Sprintf("Failed to save log: %v", err))
	}

	if err := sendLogToServer(sessionLog, config.Log.Server); err != nil {
		printError(fmt.Sprintf("Failed to send log to server: %v", err))
	}

	// Print summary
	fmt.Println()
	printInfo("EXECUTION COMPLETE")
	printInfo("==================")

	passed := 0
	failed := 0
	for _, result := range allResults {
		if result.Status == "PASSED" {
			passed++
		} else {
			failed++
		}
	}

	fmt.Printf("Total duration: %s\n", totalDuration.Round(100*time.Millisecond))
	fmt.Printf("Tests: %d passed, %d failed\n", passed, failed)

	if failed > 0 {
		os.Exit(1)
	}
}
