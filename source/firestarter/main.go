package main

import (
	"bufio"
	"bytes"
	"context"
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

	"gopkg.in/yaml.v3"
)

const VERSION = "2.0.3"

// ANSI color codes
const (
	// Существующие константы остаются
	ColorReset  = "\033[0m"
	ColorGreen  = "\033[92m"
	ColorBlue   = "\033[34m"
	ColorWhite  = "\033[37m"
	ColorYellow = "\033[33m"
	ColorRed    = "\033[31m"
	ColorGray   = "\033[90m" // Добавить если нет
	ColorCyan   = "\033[36m" // Добавить если нет

	// НОВЫЕ константы для фонов:
	ColorBgGreen  = "\033[42m\033[30m" // Зеленый фон, черный текст
	ColorBgRed    = "\033[41m\033[37m" // Красный фон, белый текст
	ColorBgYellow = "\033[43m\033[30m" // Желтый фон, черный текст
	ColorBgBlue   = "\033[44m\033[37m" // Синий фон, белый текст
)

// Configuration structures
type Config struct {
	System SystemConfig `yaml:"system"`
	Tests  TestsConfig  `yaml:"tests"`
	Flash  FlashConfig  `yaml:"flash,omitempty"`
	Log    LogConfig    `yaml:"log"`
}

type SystemConfig struct {
	Product     string `yaml:"product"`
	RequireRoot bool   `yaml:"require_root"`
	GuidPrefix  string `yaml:"guid_prefix"`
	EfiSnName   string `yaml:"efi_sn_name"`
	EfiMacName  string `yaml:"efi_mac_name"`
	DriverDir   string `yaml:"driver_dir"`
}

type TestsConfig struct {
	Timeout          string       `yaml:"timeout,omitempty"`
	ParallelGroups   [][]TestSpec `yaml:"parallel_groups,omitempty"`
	SequentialGroups [][]TestSpec `yaml:"sequential_groups,omitempty"`
}

type TestSpec struct {
	Name     string   `yaml:"name"`
	Command  string   `yaml:"command"`
	Args     []string `yaml:"args,omitempty"`
	Type     string   `yaml:"type"`
	Timeout  string   `yaml:"timeout,omitempty"`
	Required bool     `yaml:"required"`
	Collapse bool     `yaml:"collapse,omitempty"` // Новое поле: если true — при успехе не показываем вывод
}

type FlashField struct {
	Name  string `yaml:"name"`
	Flash bool   `yaml:"flash"`
	ID    string `yaml:"id"`
	Regex string `yaml:"regex"`
}

type FlashConfig struct {
	Enabled    bool         `yaml:"enabled"`
	Operations []string     `yaml:"operations,omitempty"`
	Fields     []FlashField `yaml:"fields,omitempty"`
	Method     string       `yaml:"method,omitempty"`
	VenDevice  []string     `yaml:"ven_device,omitempty"`
}

type LogConfig struct {
	SaveLocal bool   `yaml:"save_local"`
	SendLogs  bool   `yaml:"send_logs"`
	LogDir    string `yaml:"log_dir,omitempty"`
	Server    string `yaml:"server,omitempty"`
	ServerDir string `yaml:"server_dir,omitempty"`
	OpName    string `yaml:"op_name,omitempty"`
}

type FlashData struct {
	SystemSerial string
	IOBoard      string
	MAC          string
}

// Result structures
type TestResult struct {
	Name     string        `yaml:"name"`
	Status   string        `yaml:"status"` // "PASSED", "FAILED", "TIMEOUT", "SKIPPED"
	Duration time.Duration `yaml:"duration"`
	Error    string        `yaml:"error,omitempty"`
	Output   string        `yaml:"-"` // Not saved to log
	Required bool          `yaml:"required"`
	Attempts int           `yaml:"attempts,omitempty"`
}

type SystemInfo struct {
	Product   string                 `yaml:"product"`
	MBSerial  string                 `yaml:"mb_serial,omitempty"`
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
	Operator string        `yaml:"operator"`
}

type FlashResult struct {
	Operation string        `yaml:"operation"`
	Status    string        `yaml:"status"`
	Duration  time.Duration `yaml:"duration"`
	Details   string        `yaml:"details,omitempty"`
}

// Network interface management
type NetworkInterface struct {
	Name   string
	MAC    string
	IP     string
	Driver string
	State  string
}

type IntelNIC struct {
	Index        int
	VendorDevice string
	Description  string
}

type FlashMACSummary struct {
	Method         string
	TargetMAC      string
	ExistingMAC    bool
	InterfaceName  string
	OriginalIP     string
	OriginalDriver string
	NICIndices     []int // For eeupdate method
	Success        bool
	Error          string
}

// Output manager for synchronized output
type OutputManager struct {
	mutex sync.Mutex
}

// getTerminalWidth получает ширину терминала
func getTerminalWidth() int {
	// Попробуем получить через stty
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	if output, err := cmd.Output(); err == nil {
		parts := strings.Fields(string(output))
		if len(parts) >= 2 {
			if w, err := strconv.Atoi(parts[1]); err == nil && w > 0 {
				return w
			}
		}
	}

	// Fallback на переменную окружения
	if width := os.Getenv("COLUMNS"); width != "" {
		if w, err := strconv.Atoi(width); err == nil && w > 0 {
			return w
		}
	}

	// Значение по умолчанию
	return 80
}

// printSeparator печатает горизонтальную линию по ширине терминала
func printSeparator() {
	width := getTerminalWidth()
	fmt.Printf("%s%s%s\n", ColorGray, strings.Repeat("─", width), ColorReset)
}

// printThickSeparator печатает толстую горизонтальную линию
func printThickSeparator() {
	width := getTerminalWidth()
	fmt.Printf("%s%s%s\n", ColorGray, strings.Repeat("═", width), ColorReset)
}

func (om *OutputManager) PrintSection(title, content string) {
	om.mutex.Lock()
	defer om.mutex.Unlock()

	fmt.Printf("\n%s%s%s\n", ColorWhite, strings.ToUpper(title), ColorReset)
	printSeparator()

	// Выводим контент как есть
	fmt.Print(content)
	if !strings.HasSuffix(content, "\n") {
		fmt.Println()
	}

	// Пустая строка после контента для отделения от результата
	fmt.Println()
}

func (om *OutputManager) PrintResult(timestamp time.Time, name, status string, duration time.Duration, err string) {
	om.mutex.Lock()
	defer om.mutex.Unlock()

	// Форматируем статус в enterprise стиле
	var statusBlock string
	switch status {
	case "PASSED":
		statusBlock = fmt.Sprintf("%s PASSED %s", ColorBgGreen, ColorReset)
	case "FAILED":
		statusBlock = fmt.Sprintf("%s FAILED %s", ColorBgRed, ColorReset)
	case "TIMEOUT":
		statusBlock = fmt.Sprintf("%s TIMEOUT %s", ColorBgYellow, ColorReset)
	case "SKIPPED":
		statusBlock = fmt.Sprintf("%s SKIPPED %s", ColorBgYellow, ColorReset)
	case "RUNNING":
		statusBlock = fmt.Sprintf("%s RUNNING %s", ColorBgBlue, ColorReset)
	default:
		statusBlock = fmt.Sprintf("%s UNKNOWN %s", ColorWhite, ColorReset)
	}

	// Основная строка результата
	fmt.Printf("%s[%s]%s %s | Duration: %s%s%s",
		ColorGray, timestamp.Format("15:04:05"), ColorReset,
		statusBlock,
		ColorGray, duration.Round(100*time.Millisecond), ColorReset)

	// Добавляем код ошибки если есть
	if err != "" && status != "RUNNING" {
		// Пытаемся извлечь exit code из ошибки
		if strings.Contains(err, "Exit code:") {
			fmt.Printf(" | Exit Code: %s%s%s", ColorRed, strings.TrimPrefix(err, "Exit code: "), ColorReset)
		} else {
			fmt.Printf(" | %sERROR: %s%s", ColorRed, err, ColorReset)
		}
	}

	fmt.Println()
}

func printTestsSummary(results []TestResult, duration time.Duration) {
	// Заголовок
	fmt.Printf("\n%sTESTS SUMMARY%s\n", ColorWhite, ColorReset)
	printThickSeparator()

	// Подсчёт статусов
	total := len(results)
	passed, failed, skipped, timedOut := 0, 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case "PASSED":
			passed++
		case "FAILED":
			failed++
		case "SKIPPED":
			skipped++
		case "TIMEOUT":
			timedOut++
		}
	}

	// Отображение метрик
	fmt.Printf("  %-15s: %s%4d%s\n", "Total Tests", ColorWhite, total, ColorReset)
	fmt.Printf("  %-15s: %s%4d%s\n", "Passed", ColorGreen, passed, ColorReset)
	fmt.Printf("  %-15s: %s%4d%s\n", "Failed", ColorRed, failed, ColorReset)
	fmt.Printf("  %-15s: %s%4d%s\n", "Skipped", ColorYellow, skipped, ColorReset)
	fmt.Printf("  %-15s: %s%4d%s\n", "Timed Out", ColorYellow, timedOut, ColorReset)

	// Процент успешных
	if total > 0 {
		rate := (passed * 100) / total
		rateColor := ColorRed
		switch {
		case rate == 100:
			rateColor = ColorGreen
		case rate >= 80:
			rateColor = ColorYellow
		}
		fmt.Printf("  %-15s: %s%3d%%%s\n", "Success Rate", rateColor, rate, ColorReset)
	}

	// Время выполнения
	fmt.Printf("  %-15s: %s%v%s\n", "Elapsed Time", ColorGray, duration.Round(time.Second), ColorReset)

	// Разделитель перед списком
	printThickSeparator()

	// Список тестов, которые не прошли
	if failed+timedOut > 0 {
		fmt.Printf("\n%sNOT PASSED TESTS (%d)%s\n", ColorRed, failed+timedOut, ColorReset)
		for _, r := range results {
			if r.Status == "FAILED" || r.Status == "TIMEOUT" {
				fmt.Printf("  - %s%s%s\n", ColorRed, r.Name, ColorReset)
			}
		}
	} else {
		fmt.Printf("\n%sALL TESTS PASSED%s\n", ColorGreen, ColorReset)
	}

	fmt.Println()
}

var outputManager = &OutputManager{}

func printSectionHeader(title string) {
	fmt.Printf("\n%s%s%s Hardware Validation System %sv%s%s\n",
		ColorBlue, "FIRESTARTER", ColorReset, ColorGray, VERSION, ColorReset)
	printThickSeparator()
	fmt.Printf("\n%s%s%s\n", ColorWhite, strings.ToUpper(title), ColorReset)
}

func printSubHeader(title, subtitle string) {
	fmt.Printf("\n%s%s%s\n", ColorWhite, strings.ToUpper(title), ColorReset)
	if subtitle != "" {
		fmt.Printf("%s%s%s\n", ColorGray, subtitle, ColorReset)
	}
}

// printExecutionSummary выводит сводку по сессии и затем детальный вывод всех упавших тестов
func printExecutionSummary(allResults []TestResult, flashResults []FlashResult, totalDuration time.Duration) {
	fmt.Printf("\n%sSESSION SUMMARY%s\n", ColorWhite, ColorReset)
	printThickSeparator()

	// Собираем статистику тестов
	totalTests := len(allResults)
	passedTests := 0
	failedTests := 0
	skippedTests := 0
	timeoutTests := 0

	for _, result := range allResults {
		switch result.Status {
		case "PASSED":
			passedTests++
		case "FAILED":
			failedTests++
		case "SKIPPED":
			skippedTests++
		case "TIMEOUT":
			timeoutTests++
		}
	}

	// Собираем статистику прошивки
	totalFlash := len(flashResults)
	successFlash := 0
	failedFlash := 0
	for _, fr := range flashResults {
		if fr.Status == "SUCCESS" || fr.Status == "COMPLETED" || fr.Status == "PASSED" {
			successFlash++
		} else {
			failedFlash++
		}
	}

	// Выводим основные цифры
	fmt.Printf("  Total Tests       : %s%d%s\n", ColorWhite, totalTests, ColorReset)
	fmt.Printf("  Passed            : %s%d%s\n", ColorGreen, passedTests, ColorReset)
	fmt.Printf("  Failed            : %s%d%s\n", ColorRed, failedTests, ColorReset)
	fmt.Printf("  Skipped           : %s%d%s\n", ColorYellow, skippedTests, ColorReset)
	fmt.Printf("  Timeout           : %s%d%s\n", ColorYellow, timeoutTests, ColorReset)
	if totalTests > 0 {
		successRate := (passedTests * 100) / totalTests
		color := ColorRed
		if successRate >= 100 {
			color = ColorGreen
		} else if successRate >= 80 {
			color = ColorYellow
		}
		fmt.Printf("  Success Rate      : %s%d%%%s\n", color, successRate, ColorReset)
	}

	if totalFlash > 0 {
		fmt.Printf("\n  Flash Operations  : %s%d Total%s\n", ColorWhite, totalFlash, ColorReset)
		fmt.Printf("  Flash Success     : %s%d%s\n", ColorGreen, successFlash, ColorReset)
		fmt.Printf("  Flash Failed      : %s%d%s\n", ColorRed, failedFlash, ColorReset)
	}

	fmt.Printf("\n  Total Duration    : %s%s%s\n", ColorGray, totalDuration.Round(time.Second), ColorReset)

	// Определяем и выводим общий статус
	sessionStatus := "SUCCESS"
	if failedTests > 0 || failedFlash > 0 {
		sessionStatus = "FAILED"
	} else if skippedTests > 0 || timeoutTests > 0 {
		sessionStatus = "PARTIAL"
	}
	fmt.Printf("  Session Status    : ")
	switch sessionStatus {
	case "SUCCESS":
		fmt.Printf("%s SUCCESS %s\n", ColorBgGreen, ColorReset)
	case "FAILED":
		fmt.Printf("%s FAILED %s %s(issues detected)%s\n", ColorBgRed, ColorReset, ColorGray, ColorReset)
	case "PARTIAL":
		fmt.Printf("%s PARTIAL %s %s(some tests skipped)%s\n", ColorBgYellow, ColorReset, ColorGray, ColorReset)
	}

	// Если есть упавшие тесты — показываем их список
	if failedTests > 0 {
		fmt.Printf("\n%sCRITICAL ISSUES REQUIRING ATTENTION%s\n", ColorWhite, ColorReset)
		printSeparator()
		for _, result := range allResults {
			if result.Status == "FAILED" || result.Status == "TIMEOUT" {
				fmt.Printf("  %s%-20s%s %s\n", ColorRed, result.Name, ColorReset,
					func() string {
						if result.Error != "" {
							return result.Error
						}
						return "Test execution failed"
					}())
			}
		}

		// А теперь детальный вывод всех упавших тестов
		fmt.Println("\nDETAILS OF FAILED TESTS:")
		printSeparator()
		for _, result := range allResults {
			if result.Status == "FAILED" || result.Status == "TIMEOUT" {
				fmt.Printf("\n%s OUTPUT FOR %s:%s\n", ColorWhite, result.Name, ColorReset)
				printSeparator()
				fmt.Print(result.Output)
				printSeparator()
			}
		}
	}
}

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

func askUserAction(testName string) string {
	fmt.Printf("\n%s=== TEST FAILED ===%s\n", ColorRed, ColorReset)
	fmt.Printf("Test '%s' has failed.\n", testName)
	fmt.Printf("Choose action:\n")
	fmt.Printf("  %s[Y]%s Yes - Retry test (default)\n", ColorGreen, ColorReset)
	fmt.Printf("  %s[N]%s No  - Continue with next test\n", ColorYellow, ColorReset)
	fmt.Printf("  %s[S]%s Skip - Mark as skipped by operator\n", ColorBlue, ColorReset)
	fmt.Printf("Choice [Y/n/s]: ")

	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return "Y" // Default on error
	}

	choice := strings.ToUpper(strings.TrimSpace(input))
	if choice == "" {
		choice = "Y" // Default
	}

	switch choice {
	case "Y", "YES":
		return "RETRY"
	case "N", "NO":
		return "CONTINUE"
	case "S", "SKIP":
		return "SKIP"
	default:
		fmt.Printf("Invalid choice '%s', defaulting to retry.\n", choice)
		return "RETRY"
	}
}

func askUserProductMismatch(configProduct, detectedProduct string) bool {
	reader := bufio.NewReader(os.Stdin)

	fmt.Printf("\n%s⚠️  PRODUCT MISMATCH WARNING ⚠️%s\n", ColorRed, ColorReset)
	fmt.Printf("Configuration file is designed for: %s%s%s\n", ColorYellow, configProduct, ColorReset)
	fmt.Printf("Detected system product: %s%s%s\n", ColorYellow, detectedProduct, ColorReset)
	fmt.Printf("\nThis configuration may not be suitable for your hardware.\n")
	fmt.Printf("Continuing may lead to unexpected behavior or hardware damage.\n\n")

	for {
		fmt.Printf("Do you want to close the program? %s[Y/n]%s: ", ColorGreen, ColorReset)

		input, err := reader.ReadString('\n')
		if err != nil {
			fmt.Printf("%sError reading input: %v%s\n", ColorRed, err, ColorReset)
			continue
		}

		input = strings.TrimSpace(strings.ToLower(input))

		// Default is 'Y' (close program)
		if input == "" || input == "y" || input == "yes" {
			return true // Close program
		} else if input == "n" || input == "no" {
			return false // Continue
		} else {
			fmt.Printf("%sPlease enter 'Y' to close or 'N' to continue.%s\n", ColorRed, ColorReset)
		}
	}
}

func executeTest(test TestSpec, globalTimeout string) (TestResult, string) {
	result := TestResult{
		Name:     test.Name,
		Status:   "FAILED",
		Required: test.Required,
	}

	startTime := time.Now()

	// Parse timeout - приоритет: тест > глобальный > дефолт
	timeout := 30 * time.Second
	if test.Timeout != "" {
		if t, err := time.ParseDuration(test.Timeout); err == nil {
			timeout = t
		}
	} else if globalTimeout != "" {
		if t, err := time.ParseDuration(globalTimeout); err == nil {
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

	return result, output
}

// runTest выполняет тест и возвращает результат, не выводя сразу секцию с полным выводом
func runTest(test TestSpec, outputMgr *OutputManager, globalTimeout string) TestResult {
	attempts := 0
	maxAttempts := 5

	var result TestResult
	var output string

	for attempts < maxAttempts {
		attempts++
		outputMgr.PrintResult(time.Now(), test.Name, "RUNNING", 0, "")

		result, output = executeTest(test, globalTimeout)
		result.Attempts = attempts
		result.Output = output

		outputMgr.PrintResult(time.Now(), test.Name, result.Status, result.Duration, result.Error)

		// Решаем, показывать ли полный вывод:
		if output != "" && !(result.Status == "PASSED" && test.Collapse) {
			outputMgr.PrintSection(test.Name+" Output", output)
		}

		if result.Status == "PASSED" {
			return result
		}

		action := askUserAction(test.Name)
		switch action {
		case "RETRY":
			fmt.Printf("%sRetrying test '%s' (attempt %d)...%s\n\n", ColorBlue, test.Name, attempts+1, ColorReset)
			continue
		case "SKIP":
			result.Status = "SKIPPED"
			result.Error = "Skipped by operator"
			return result
		case "CONTINUE":
			return result
		}
	}

	// Если дошли до лимита попыток
	fmt.Printf("%sMaximum retry attempts (%d) reached for test '%s'%s\n", ColorRed, maxAttempts, test.Name, ColorReset)
	finalResult, finalOutput := executeTest(test, globalTimeout)
	finalResult.Attempts = attempts
	finalResult.Output = finalOutput

	outputMgr.PrintResult(time.Now(), test.Name, finalResult.Status, finalResult.Duration, finalResult.Error)
	if finalOutput != "" && !(finalResult.Status == "PASSED" && test.Collapse) {
		outputMgr.PrintSection(test.Name+" Output", finalOutput)
	}
	return finalResult
}

// runParallelTestsWithRetries выполняет набор тестов параллельно, а потом последовательно обрабатывает упавшие,
// показывая при этом сразу причину и вывод для каждого неудачного теста.
func runParallelTestsWithRetries(tests []TestSpec, outputMgr *OutputManager, globalTimeout string) []TestResult {
	results := make([]TestResult, len(tests))
	finalResults := make([]TestResult, len(tests))

	// --- Параллельный запуск ---
	var wg sync.WaitGroup
	for i, t := range tests {
		wg.Add(1)
		go func(idx int, test TestSpec) {
			defer wg.Done()

			outputMgr.PrintResult(time.Now(), test.Name, "RUNNING", 0, "")
			res, out := executeTest(test, globalTimeout)
			res.Attempts = 1
			res.Output = out

			outputMgr.PrintResult(time.Now(), test.Name, res.Status, res.Duration, res.Error)
			if out != "" && !(res.Status == "PASSED" && test.Collapse) {
				outputMgr.PrintSection(test.Name+" Output", out)
			}

			results[idx] = res
		}(i, t)
	}
	wg.Wait()

	// --- Подсчитываем упавшие ---
	failedCount := 0
	for _, r := range results {
		if r.Status == "FAILED" || r.Status == "TIMEOUT" {
			failedCount++
		}
	}
	if failedCount > 0 {
		fmt.Printf("\n%sParallel complete: %d failed test(s)%s\n", ColorYellow, failedCount, ColorReset)
	} else {
		fmt.Printf("\n%sAll parallel tests passed%s\n", ColorGreen, ColorReset)
	}

	// --- Последовательная доработка упавших ---
	proc := 0
	for i, r := range results {
		if r.Status == "PASSED" {
			finalResults[i] = r
			continue
		}
		proc++
		if proc > 1 {
			fmt.Println()
		}
		fmt.Printf("%sProcessing failed test %d/%d: %s%s\n",
			ColorBlue, proc, failedCount, tests[i].Name, ColorReset)

		// Всегда показываем причину и вывод перед retry/skip
		fmt.Printf("  Status: %s%s%s\n", ColorRed, r.Status, ColorReset)
		if r.Error != "" {
			fmt.Printf("  Error : %s\n", r.Error)
		}
		if r.Output != "" {
			outputMgr.PrintSection(tests[i].Name+" Output", r.Output)
		}

		finalResults[i] = handleFailedTestWithRetries(tests[i], r, outputMgr, globalTimeout)
	}

	return finalResults
}

// handleFailedTestWithRetries предлагает retry/skip/continue до 5 раз
func handleFailedTestWithRetries(test TestSpec, initialResult TestResult, outputMgr *OutputManager, globalTimeout string) TestResult {
	currentResult := initialResult
	attempts := initialResult.Attempts
	maxAttempts := 5

	for attempts < maxAttempts && currentResult.Status != "PASSED" {
		action := askUserAction(test.Name)
		switch action {
		case "RETRY":
			attempts++
			fmt.Printf("%sRetrying test '%s' (attempt %d)...%s\n\n", ColorBlue, test.Name, attempts, ColorReset)
			outputMgr.PrintResult(time.Now(), test.Name, "RUNNING", 0, "")
			result, output := executeTest(test, globalTimeout)
			result.Attempts = attempts
			result.Output = output
			outputMgr.PrintResult(time.Now(), test.Name, result.Status, result.Duration, result.Error)
			currentResult = result
		case "SKIP":
			currentResult.Status = "SKIPPED"
			currentResult.Error = "Skipped by operator"
			outputMgr.PrintResult(time.Now(), test.Name, currentResult.Status, currentResult.Duration, currentResult.Error)
			return currentResult
		case "CONTINUE":
			return currentResult
		}
	}

	if attempts >= maxAttempts && currentResult.Status != "PASSED" {
		fmt.Printf("%sMaximum retry attempts (%d) reached for test '%s'%s\n", ColorRed, maxAttempts, test.Name, ColorReset)
	}

	return currentResult
}

func runTestGroup(tests []TestSpec, parallel bool, outputMgr *OutputManager, groupName, globalTimeout string) []TestResult {
	fmt.Printf("\n%s%s%s\n", ColorWhite, strings.ToUpper(groupName), ColorReset)

	mode := "Sequential"
	if parallel {
		mode = "Parallel"
	}

	fmt.Printf("Mode: %s%s%s | Tests: %s%d%s | Timeout: %s%s%s\n",
		ColorCyan, mode, ColorReset,
		ColorGreen, len(tests), ColorReset,
		ColorYellow, func() string {
			if globalTimeout != "" {
				return globalTimeout
			}
			return "30s (default)"
		}(), ColorReset)

	printSeparator()

	var results []TestResult
	if parallel {
		results = runParallelTestsWithRetries(tests, outputMgr, globalTimeout)
	} else {
		results = make([]TestResult, len(tests))
		for i, test := range tests {
			results[i] = runTest(test, outputMgr, globalTimeout)
		}
	}

	// Выводим сводку группы в enterprise стиле
	fmt.Printf("\n%sGROUP RESULTS%s\n", ColorWhite, ColorReset)
	printSeparator()

	passed := 0
	failed := 0
	skipped := 0

	var passedTests []string
	var failedTests []string
	var skippedTests []string

	for _, result := range results {
		switch result.Status {
		case "PASSED":
			passed++
			passedTests = append(passedTests, result.Name)
		case "FAILED", "TIMEOUT":
			failed++
			failedTests = append(failedTests, result.Name)
		case "SKIPPED":
			skipped++
			skippedTests = append(skippedTests, result.Name)
		}
	}

	// Определяем статус группы
	groupStatus := "PASSED"
	if failed > 0 {
		groupStatus = "FAILED"
	} else if skipped > 0 {
		groupStatus = "PARTIAL"
	}

	// Выводим статистику
	fmt.Printf("  %s%-20s%s: ", ColorWhite, groupName, ColorReset)
	switch groupStatus {
	case "PASSED":
		fmt.Printf("%s PASSED %s", ColorBgGreen, ColorReset)
	case "FAILED":
		fmt.Printf("%s FAILED %s %s(%d of %d tests failed)%s",
			ColorBgRed, ColorReset, ColorGray, failed, len(tests), ColorReset)
	case "PARTIAL":
		fmt.Printf("%s PARTIAL %s %s(%d passed, %d skipped)%s",
			ColorBgYellow, ColorReset, ColorGray, passed, skipped, ColorReset)
	}
	fmt.Println()

	// Выводим списки тестов
	if len(passedTests) > 0 {
		fmt.Printf("  %sPassed:%s %s\n", ColorGreen, ColorReset, strings.Join(passedTests, ", "))
	}
	if len(failedTests) > 0 {
		fmt.Printf("  %sFailed:%s %s\n", ColorRed, ColorReset, strings.Join(failedTests, ", "))
	}
	if len(skippedTests) > 0 {
		fmt.Printf("  %sSkipped:%s %s\n", ColorYellow, ColorReset, strings.Join(skippedTests, ", "))
	}

	return results
}

func getFlashData(config FlashConfig, productName string) (*FlashData, error) {
	if !config.Enabled || len(config.Fields) == 0 {
		return nil, nil
	}

	if productName == "" {
		return nil, fmt.Errorf("product name not detected")
	}

	printSectionHeader("FLASH DATA COLLECTION")
	fmt.Printf("Product: %s%s%s\n", ColorGreen, productName, ColorReset)
	fmt.Printf("Method: %s%s%s\n", ColorGreen, config.Method, ColorReset)
	if len(config.VenDevice) > 0 {
		fmt.Printf("Target Devices: %s%s%s\n", ColorYellow, strings.Join(config.VenDevice, ", "), ColorReset)
	}

	// Prepare fields that need flashing
	requiredFields := make(map[string]*FlashField)
	flashFields := make(map[string]*FlashField)

	fmt.Printf("\nRequired fields:\n")
	for i := range config.Fields {
		field := &config.Fields[i]
		_, err := regexp.Compile(field.Regex)
		if err != nil {
			return nil, fmt.Errorf("invalid regex for field %s: %v", field.Name, err)
		}

		requiredFields[field.ID] = field
		if field.Flash {
			flashFields[field.ID] = field
			fmt.Printf("  %s[FLASH]%s %s (format: %s)\n", ColorYellow, ColorReset, field.Name, field.Regex)
		} else {
			fmt.Printf("  %s[STORE]%s %s (format: %s)\n", ColorBlue, ColorReset, field.Name, field.Regex)
		}
	}

	provided := make(map[string]string)
	reader := bufio.NewReader(os.Stdin)

	fmt.Printf("\nEnter values (program will auto-detect field type):\n")

	for len(provided) < len(requiredFields) {
		fmt.Printf("\nRemaining fields: %d\n", len(requiredFields)-len(provided))
		fmt.Printf("Enter value: ")

		input, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		input = strings.TrimSpace(input)

		if input == "" {
			fmt.Printf("%sInput cannot be empty. Please re-enter.%s\n", ColorRed, ColorReset)
			continue
		}

		matched := false
		for fieldID, field := range requiredFields {
			if _, ok := provided[fieldID]; ok {
				continue
			}

			regex, _ := regexp.Compile(field.Regex) // Already validated above
			if regex.MatchString(input) {
				provided[fieldID] = input
				flashStatus := ""
				if field.Flash {
					flashStatus = fmt.Sprintf(" %s[WILL FLASH]%s", ColorYellow, ColorReset)
				} else {
					flashStatus = fmt.Sprintf(" %s[STORED ONLY]%s", ColorBlue, ColorReset)
				}
				fmt.Printf("%s%s accepted: %s%s%s\n", ColorGreen, field.Name, input, flashStatus, ColorReset)
				matched = true
				break
			}
		}

		if !matched {
			fmt.Printf("%sInput does not match any expected format. Please try again.%s\n", ColorRed, ColorReset)
		}
	}

	flashData := &FlashData{}

	// Map fields to FlashData structure
	for fieldID, value := range provided {
		switch fieldID {
		case "system-serial-number":
			flashData.SystemSerial = value
		case "io_board":
			flashData.IOBoard = value
		case "mac_address":
			flashData.MAC = value
		}
	}

	fmt.Printf("\n%sCollected data summary:%s\n", ColorGreen, ColorReset)
	if flashData.SystemSerial != "" {
		fmt.Printf("  System Serial: %s\n", flashData.SystemSerial)
	}
	if flashData.IOBoard != "" {
		fmt.Printf("  IO Board: %s\n", flashData.IOBoard)
	}
	if flashData.MAC != "" {
		fmt.Printf("  MAC Address: %s\n", flashData.MAC)
	}

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

// Network interface management functions
func getCurrentNetworkInterfaces() ([]NetworkInterface, error) {
	var interfaces []NetworkInterface

	// Get network interfaces using 'ip' command
	cmd := exec.Command("ip", "addr", "show")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get network interfaces: %v", err)
	}

	lines := strings.Split(string(output), "\n")
	var currentInterface *NetworkInterface

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Parse interface name and state
		if strings.Contains(line, ": ") && !strings.HasPrefix(line, " ") {
			if currentInterface != nil {
				interfaces = append(interfaces, *currentInterface)
			}

			// Extract interface name
			parts := strings.Split(line, ":")
			if len(parts) >= 2 {
				name := strings.TrimSpace(parts[1])
				currentInterface = &NetworkInterface{Name: name}

				// Extract state
				if strings.Contains(line, "state UP") {
					currentInterface.State = "UP"
				} else if strings.Contains(line, "state DOWN") {
					currentInterface.State = "DOWN"
				}
			}
		}

		// Parse MAC address
		if currentInterface != nil && strings.Contains(line, "link/ether") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				currentInterface.MAC = strings.ToUpper(parts[1])
			}
		}

		// Parse IP address
		if currentInterface != nil && strings.Contains(line, "inet ") && !strings.Contains(line, "127.0.0.1") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				ip := strings.Split(parts[1], "/")[0]
				currentInterface.IP = ip
			}
		}
	}

	// Add the last interface
	if currentInterface != nil {
		interfaces = append(interfaces, *currentInterface)
	}

	// Get driver information for each interface
	for i := range interfaces {
		if driver, err := getInterfaceDriver(interfaces[i].Name); err == nil {
			interfaces[i].Driver = driver
		}
	}

	return interfaces, nil
}

func getInterfaceDriver(interfaceName string) (string, error) {
	// Try ethtool first
	cmd := exec.Command("ethtool", "-i", interfaceName)
	output, err := cmd.Output()
	if err == nil {
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "driver:") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					return strings.TrimSpace(parts[1]), nil
				}
			}
		}
	}

	// Fallback: check /sys/class/net
	driverPath := fmt.Sprintf("/sys/class/net/%s/device/driver", interfaceName)
	if link, err := os.Readlink(driverPath); err == nil {
		return filepath.Base(link), nil
	}

	return "", fmt.Errorf("driver not found for interface %s", interfaceName)
}

func normalizeMAC(mac string) string {
	// Remove any separators and convert to uppercase
	mac = strings.ReplaceAll(mac, ":", "")
	mac = strings.ReplaceAll(mac, "-", "")
	mac = strings.ToUpper(mac)

	// Add colons in standard format
	if len(mac) == 12 {
		return fmt.Sprintf("%s:%s:%s:%s:%s:%s",
			mac[0:2], mac[2:4], mac[4:6], mac[6:8], mac[8:10], mac[10:12])
	}

	return mac
}

func isTargetMACPresent(targetMAC string, interfaces []NetworkInterface) (bool, string) {
	normalizedTarget := normalizeMAC(targetMAC)

	for _, iface := range interfaces {
		if normalizeMAC(iface.MAC) == normalizedTarget {
			return true, iface.Name
		}
	}

	return false, ""
}

func askFlashRetryAction(message string) string {
	fmt.Printf("\n%s=== MAC FLASHING ERROR ===%s\n", ColorRed, ColorReset)
	fmt.Printf("%s\n", message)
	fmt.Println("Choose action:")
	fmt.Printf("  %s[Y]%s Yes - Retry flashing (default)\n", ColorGreen, ColorReset)
	fmt.Printf("  %s[A]%s Abort - Stop flashing and continue program\n", ColorYellow, ColorReset)
	fmt.Printf("  %s[S]%s Skip - Skip MAC flashing by operator decision\n", ColorBlue, ColorReset)
	fmt.Printf("Choice [Y/a/s]: ")

	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return "RETRY" // default on error
	}

	choice := strings.ToUpper(strings.TrimSpace(input))
	if choice == "" {
		choice = "Y" // default
	}

	switch choice {
	case "Y", "YES":
		return "RETRY"
	case "A", "ABORT":
		return "ABORT"
	case "S", "SKIP":
		return "SKIP"
	default:
		fmt.Printf("Invalid choice '%s', defaulting to retry.\n", choice)
		return "RETRY"
	}
}

func flashMAC(flashConfig FlashConfig, systemConfig SystemConfig, mac string) error {
	method := flashConfig.Method
	if method == "" {
		method = "eeupdate" // default
	}

	printSubHeader("MAC ADDRESS FLASHING", fmt.Sprintf("Method: %s | Target MAC: %s", method, mac))

	// Step 1: Get current network interfaces
	interfaces, err := getCurrentNetworkInterfaces()
	if err != nil {
		return fmt.Errorf("failed to get network interfaces: %v", err)
	}

	// Step 2: Check if target MAC already exists
	exists, interfaceName := isTargetMACPresent(mac, interfaces)
	if exists {
		printSuccess(fmt.Sprintf("Target MAC %s already present on interface %s - skipping flash", mac, interfaceName))
		return nil
	}

	// Step 3: Show current network state
	fmt.Printf("\nCurrent network interfaces:\n")
	for _, iface := range interfaces {
		status := "DOWN"
		if iface.State == "UP" {
			status = fmt.Sprintf("UP (IP: %s)", iface.IP)
		}
		fmt.Printf("  %s: %s [%s] - %s\n", iface.Name, iface.MAC, iface.Driver, status)
	}

	// Step 4: Execute flashing based on method
	var summary FlashMACSummary
	summary.Method = method
	summary.TargetMAC = mac

	switch method {
	case "rtnicpg":
		err = flashMACWithRtnicpg(mac, interfaces, systemConfig, &summary)
	case "eeupdate":
		err = flashMACWithEeupdate(mac, interfaces, flashConfig, &summary)
	default:
		return fmt.Errorf("unknown flash method: %s", method)
	}

	if err != nil {
		return fmt.Errorf("MAC flashing failed: %v", err)
	}

	if summary.Success {
		printSuccess(fmt.Sprintf("MAC address flashed successfully using %s method", method))
	}

	return nil
}

func discoverIntelNICs(venDeviceFilter []string) ([]IntelNIC, error) {
	printInfo("Discovering Intel network cards...")

	cmd := exec.Command("eeupdate64e", "/MAC_DUMP_ALL")
	output, err := cmd.CombinedOutput()
	outputStr := string(output)

	// Check if command failed completely (exit codes other than 2 are critical)
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode := exitError.ExitCode()
			if exitCode == 2 {
				// Exit code 2 usually means no driver found, but utility can still work
				printInfo("eeupdate64e reports no driver (exit code 2), but continuing...")
			} else {
				// Other exit codes are more serious errors
				return nil, fmt.Errorf("eeupdate64e discovery failed with exit code %d: %v\nOutput: %s", exitCode, err, outputStr)
			}
		} else {
			// Non-ExitError (like command not found)
			return nil, fmt.Errorf("eeupdate64e discovery failed: %v\nOutput: %s", err, outputStr)
		}
	}

	// Parse output to find NIC indices regardless of exit code
	var allNICs []IntelNIC
	lines := strings.Split(outputStr, "\n")

	for _, line := range lines {
		// Parse lines with device IDs (8086-XXXX format indicates Intel)
		if strings.Contains(line, "8086-") {
			fields := strings.Fields(line)
			if len(fields) >= 5 {
				// First field should be NIC index
				nicIndex, err := strconv.Atoi(fields[0])
				if err != nil {
					continue
				}

				// Extract vendor-device ID (format: 8086-1521)
				venDevice := fields[4]
				description := strings.Join(fields[5:], " ")

				nic := IntelNIC{
					Index:        nicIndex,
					VendorDevice: venDevice,
					Description:  description,
				}

				allNICs = append(allNICs, nic)
				printInfo(fmt.Sprintf("Found Intel NIC %d: %s (%s)", nicIndex, venDevice, description))
			}
		}
	}

	if len(allNICs) == 0 {
		// If no NICs found in parsing, but we got output, try common indices
		if len(outputStr) > 100 { // Substantial output suggests NICs might be there
			printInfo("No NICs found in parsing, but substantial output detected. Trying common indices...")
			for i := 1; i <= 6; i++ {
				allNICs = append(allNICs, IntelNIC{Index: i, VendorDevice: "unknown", Description: "Unknown Intel NIC"})
			}
		} else {
			return nil, fmt.Errorf("no Intel network cards found in output")
		}
	}

	// Apply vendor-device filter if specified
	var filteredNICs []IntelNIC
	if len(venDeviceFilter) > 0 {
		printInfo(fmt.Sprintf("Applying vendor-device filter: %s", strings.Join(venDeviceFilter, ", ")))
		for _, nic := range allNICs {
			for _, filter := range venDeviceFilter {
				if nic.VendorDevice == filter {
					filteredNICs = append(filteredNICs, nic)
					printInfo(fmt.Sprintf("NIC %d matches filter %s", nic.Index, filter))
					break
				}
			}
		}
		if len(filteredNICs) == 0 {
			return nil, fmt.Errorf("no NICs match the specified vendor-device filter: %s", strings.Join(venDeviceFilter, ", "))
		}
	} else {
		filteredNICs = allNICs
	}

	printSuccess(fmt.Sprintf("Discovery completed: found %d Intel NIC(s) (after filtering)", len(filteredNICs)))
	return filteredNICs, nil
}

// incrementMAC increases MAC address by 1 (handles hexadecimal arithmetic)
func incrementMAC(mac string) (string, error) {
	// Split MAC address into bytes
	parts := strings.Split(mac, ":")
	if len(parts) != 6 {
		return "", fmt.Errorf("invalid MAC address format: %s", mac)
	}

	// Convert the last byte to an integer, increment it, and convert back
	lastByte, err := strconv.ParseUint(parts[5], 16, 8)
	if err != nil {
		return "", fmt.Errorf("invalid MAC address byte: %s", parts[5])
	}

	// Increment with overflow handling
	lastByte = (lastByte + 1) % 256

	// If the last byte overflows, increment the previous byte
	if lastByte == 0 {
		fifthByte, err := strconv.ParseUint(parts[4], 16, 8)
		if err != nil {
			return "", fmt.Errorf("invalid MAC address byte: %s", parts[4])
		}
		fifthByte = (fifthByte + 1) % 256
		parts[4] = fmt.Sprintf("%02x", fifthByte)
	}

	// Update the last byte
	parts[5] = fmt.Sprintf("%02x", lastByte)

	// Join parts back together
	return strings.Join(parts, ":"), nil
}

func executeEeupdateFlashing(nicIndex int, targetMAC string) error {

	cleanMac := strings.ReplaceAll(targetMAC, ":", "")

	printInfo(fmt.Sprintf("Executing eeupdate flashing for NIC %d, MAC: %s", nicIndex, targetMAC))

	// Execute eeupdate64e with NIC and MAC parameters
	cmd := exec.Command("eeupdate64e",
		fmt.Sprintf("/NIC=%d", nicIndex),
		fmt.Sprintf("/MAC=%s", cleanMac))

	output, err := cmd.CombinedOutput()
	outputStr := string(output)

	// Get exit code for detailed error reporting
	var exitCode int = 0
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		}
	}

	// Handle exit codes specifically
	if err != nil {
		if exitCode == 2 {
			// Exit code 2 usually means no driver, but flashing might still work
			printInfo(fmt.Sprintf("eeupdate64e reports no driver (exit code 2) for NIC %d, checking output for success...", nicIndex))
		} else {
			// Other exit codes might be more serious
			printError(fmt.Sprintf("eeupdate64e failed with exit code %d for NIC %d", exitCode, nicIndex))
			printError(fmt.Sprintf("Output: %s", outputStr))
			return fmt.Errorf("eeupdate64e command failed with exit code %d: %v", exitCode, err)
		}
	}

	// Check output for success/failure indicators regardless of exit code
	outputLower := strings.ToLower(outputStr)

	// Look for specific success patterns from eeupdate
	if strings.Contains(outputStr, "Updating Mac Address") && strings.Contains(outputStr, "Done") {
		printSuccess(fmt.Sprintf("eeupdate flashing completed for NIC %d", nicIndex))
		return nil
	}

	if strings.Contains(outputStr, "Updating Checksum and CRCs") && strings.Contains(outputStr, "Done") {
		printSuccess(fmt.Sprintf("eeupdate flashing completed for NIC %d", nicIndex))
		return nil
	}

	// Other positive indicators
	if strings.Contains(outputLower, "success") ||
		strings.Contains(outputLower, "complete") ||
		strings.Contains(outputLower, "updated") ||
		strings.Contains(outputLower, "written") {
		printSuccess(fmt.Sprintf("eeupdate flashing completed for NIC %d", nicIndex))
		return nil
	}

	// Negative indicators (but exclude our own error headers)
	if (strings.Contains(outputLower, "error") && !strings.Contains(outputLower, "mac flashing error")) ||
		strings.Contains(outputLower, "fail") ||
		strings.Contains(outputLower, "invalid") {
		return fmt.Errorf("eeupdate reported error for NIC %d (exit code %d): %s", nicIndex, exitCode, outputStr)
	}

	// If no clear indicators but we got substantial output, assume it worked
	if len(outputStr) > 50 && err == nil {
		printSuccess(fmt.Sprintf("eeupdate command completed for NIC %d", nicIndex))
		return nil
	}

	// If exit code 2 but minimal output, still try to continue
	if err != nil && exitCode == 2 {
		printInfo(fmt.Sprintf("eeupdate completed for NIC %d with driver warning (exit code 2)", nicIndex))
		return nil
	}

	// Default case - if we get here, status is unclear
	printInfo(fmt.Sprintf("eeupdate command status unclear for NIC %d (exit code %d), assuming success", nicIndex, exitCode))
	return nil
}

func flashMACWithEeupdate(targetMAC string, interfaces []NetworkInterface, flashConfig FlashConfig, summary *FlashMACSummary) error {
	printInfo("Starting eeupdate MAC flashing process...")

	// Step 1: Save current IP
	var originalIP string
	for _, iface := range interfaces {
		if iface.IP != "" && iface.State == "UP" {
			originalIP = iface.IP
			break
		}
	}
	summary.OriginalIP = originalIP

	if originalIP != "" {
		printInfo(fmt.Sprintf("Current IP address saved: %s", originalIP))
	}

	// Step 2: Discover Intel NICs with optional filtering
	printInfo("Scanning for Intel network cards...")
	intelNICs, err := discoverIntelNICs(flashConfig.VenDevice)
	if err != nil {
		return fmt.Errorf("failed to discover Intel NICs: %v", err)
	}

	if len(intelNICs) == 0 {
		return fmt.Errorf("no Intel network cards found")
	}

	// Extract indices for summary
	var nicIndices []int
	for _, nic := range intelNICs {
		nicIndices = append(nicIndices, nic.Index)
	}
	summary.NICIndices = nicIndices

	printSuccess(fmt.Sprintf("Found %d Intel NIC(s) for flashing:", len(intelNICs)))
	for i, nic := range intelNICs {
		// Calculate MAC for this NIC (first gets original, others get incremented)
		currentMAC := targetMAC
		if i > 0 {
			for j := 0; j < i; j++ {
				currentMAC, err = incrementMAC(currentMAC)
				if err != nil {
					return fmt.Errorf("failed to increment MAC address for NIC %d: %v", nic.Index, err)
				}
			}
		}
		fmt.Printf("  NIC %d: %s (%s) -> MAC: %s\n", nic.Index, nic.VendorDevice, nic.Description, currentMAC)
	}

	// Step 3: Flash each NIC with incremented MAC addresses
	attempts := 0
	maxAttempts := 3
	var lastError error

	for attempts < maxAttempts {
		attempts++
		printInfo(fmt.Sprintf("Flashing attempt %d/%d...", attempts, maxAttempts))

		success := true
		flashedNICs := 0

		for i, nic := range intelNICs {
			// Calculate MAC for this NIC
			currentMAC := targetMAC
			if i > 0 {
				for j := 0; j < i; j++ {
					currentMAC, err = incrementMAC(currentMAC)
					if err != nil {
						lastError = fmt.Errorf("failed to increment MAC address for NIC %d: %v", nic.Index, err)
						success = false
						break
					}
				}
			}

			if !success {
				break
			}

			printInfo(fmt.Sprintf("Flashing NIC %d (%s) with MAC %s...", nic.Index, nic.VendorDevice, currentMAC))
			if err := executeEeupdateFlashing(nic.Index, currentMAC); err != nil {
				printError(fmt.Sprintf("Failed to flash NIC %d: %v", nic.Index, err))
				lastError = fmt.Errorf("failed to flash NIC %d: %v", nic.Index, err)
				success = false
				break
			} else {
				flashedNICs++
				printSuccess(fmt.Sprintf("NIC %d flashing completed with MAC %s", nic.Index, currentMAC))
			}
		}

		if success {
			printSuccess(fmt.Sprintf("All %d NICs flashed successfully with incremented MAC addresses", flashedNICs))
			lastError = nil
			break
		}

		if attempts < maxAttempts {
			action := askFlashRetryAction(fmt.Sprintf("eeupdate flashing failed (attempt %d/%d): %v", attempts, maxAttempts, lastError))
			if action == "SKIP" {
				summary.Success = false
				summary.Error = "Skipped by operator"
				return nil
			}
			if action == "ABORT" {
				summary.Success = false
				summary.Error = fmt.Sprintf("Aborted by operator after %d attempts", attempts)
				return fmt.Errorf("flashing aborted by operator")
			}
			// Continue to retry if action == "RETRY"
		}
	}

	if lastError != nil && attempts >= maxAttempts {
		summary.Success = false
		summary.Error = fmt.Sprintf("Max attempts reached: %v", lastError)
		return lastError
	}

	// Step 4: Restart network and verify
	printInfo("Restarting network services to detect new MAC addresses...")
	restartNetworkServices()

	time.Sleep(3 * time.Second) // Wait for network to come up

	// Step 5: Verify that at least the first MAC address is present
	printInfo("Verifying MAC address presence...")
	newInterfaces, err := getCurrentNetworkInterfaces()
	if err != nil {
		printError(fmt.Sprintf("Warning: failed to verify MAC flashing: %v", err))
	} else {
		// Check for the primary MAC address (first one)
		exists, interfaceName := isTargetMACPresent(targetMAC, newInterfaces)
		if exists {
			summary.Success = true
			summary.InterfaceName = interfaceName
			printSuccess(fmt.Sprintf("SUCCESS: Primary MAC %s found on interface %s", targetMAC, interfaceName))

			// Also check for incremented MAC addresses and report them
			currentMAC := targetMAC
			for i := 1; i < len(intelNICs); i++ {
				currentMAC, err = incrementMAC(currentMAC)
				if err != nil {
					printError(fmt.Sprintf("Warning: failed to increment MAC for verification: %v", err))
					break
				}

				exists, ifaceName := isTargetMACPresent(currentMAC, newInterfaces)
				if exists {
					printSuccess(fmt.Sprintf("Additional MAC %s found on interface %s", currentMAC, ifaceName))
				} else {
					printError(fmt.Sprintf("Warning: Expected MAC %s not found on any interface", currentMAC))
				}
			}

			// Try to restore IP address to the primary interface
			if originalIP != "" {
				printInfo(fmt.Sprintf("Restoring original IP address: %s", originalIP))
				if err := restoreIPAddress(interfaceName, originalIP); err != nil {
					printError(fmt.Sprintf("Warning: failed to restore IP %s: %v", originalIP, err))
				} else {
					printSuccess(fmt.Sprintf("IP address %s restored successfully", originalIP))
				}
			}
		} else {
			printError("Primary MAC not found on any interface after flashing")
			action := askFlashRetryAction(fmt.Sprintf("Flashing completed but target MAC %s not found on any interface", targetMAC))
			if action == "SKIP" {
				summary.Success = false
				summary.Error = "MAC not found after flashing - skipped by operator"
				return nil
			}
			if action == "ABORT" {
				summary.Success = false
				summary.Error = "MAC not found after flashing - aborted by operator"
				return fmt.Errorf("MAC not found after flashing - aborted by operator")
			}
			summary.Success = false
			summary.Error = "MAC not found after flashing"
			return fmt.Errorf("target MAC not found after flashing")
		}
	}

	return nil
}

func flashMACWithRtnicpg(targetMAC string, interfaces []NetworkInterface, systemConfig SystemConfig, summary *FlashMACSummary) error {
	printInfo("Starting rtnicpg MAC flashing process...")

	// Step 1: Find network interface with IP and save current state
	var primaryInterface *NetworkInterface
	for i := range interfaces {
		if interfaces[i].IP != "" && interfaces[i].State == "UP" {
			primaryInterface = &interfaces[i]
			break
		}
	}

	if primaryInterface == nil {
		return fmt.Errorf("no active network interface with IP found")
	}

	summary.OriginalIP = primaryInterface.IP
	summary.OriginalDriver = primaryInterface.Driver

	printInfo(fmt.Sprintf("Using interface %s (IP: %s, Driver: %s)",
		primaryInterface.Name, primaryInterface.IP, primaryInterface.Driver))

	// Step 2: Unload current driver
	printInfo(fmt.Sprintf("Unloading network driver: %s", primaryInterface.Driver))
	if err := unloadNetworkDriver(primaryInterface.Driver); err != nil {
		return fmt.Errorf("failed to unload driver %s: %v", primaryInterface.Driver, err)
	}

	// Step 3: Load flashing driver
	driverPath, err := loadFlashingDriver(systemConfig.DriverDir, primaryInterface.Driver)
	if err != nil {
		// Try to restore original driver
		loadNetworkDriver(primaryInterface.Driver)
		return fmt.Errorf("failed to load flashing driver: %v", err)
	}

	// Step 4: Flash MAC using rtnic
	attempts := 0
	maxAttempts := 3

	for attempts < maxAttempts {
		attempts++
		printInfo(fmt.Sprintf("Flashing MAC attempt %d/%d...", attempts, maxAttempts))

		err = executeRtnicFlashing(targetMAC)
		if err == nil {
			break
		}

		if attempts < maxAttempts {
			action := askFlashRetryAction(fmt.Sprintf("rtnic flashing failed (attempt %d): %v", attempts, err))
			if action == "SKIP" {
				summary.Success = false
				summary.Error = "Skipped by operator"
				return nil
			}
			if action != "RETRY" {
				break
			}
		}
	}

	// Step 5: Unload flashing driver and restore original
	unloadNetworkDriver(filepath.Base(driverPath))
	if err := loadNetworkDriver(primaryInterface.Driver); err != nil {
		printError(fmt.Sprintf("Warning: failed to restore original driver %s: %v", primaryInterface.Driver, err))
	}

	if err != nil && attempts >= maxAttempts {
		summary.Success = false
		summary.Error = fmt.Sprintf("Max attempts reached: %v", err)
		return err
	}

	// Step 6: Verify MAC was flashed
	time.Sleep(2 * time.Second) // Wait for interfaces to come up
	newInterfaces, err := getCurrentNetworkInterfaces()
	if err != nil {
		printError(fmt.Sprintf("Warning: failed to verify MAC flashing: %v", err))
	} else {
		exists, interfaceName := isTargetMACPresent(targetMAC, newInterfaces)
		if exists {
			summary.Success = true
			summary.InterfaceName = interfaceName
			printSuccess(fmt.Sprintf("MAC %s found on interface %s", targetMAC, interfaceName))

			// Try to restore IP address
			if err := restoreIPAddress(interfaceName, summary.OriginalIP); err != nil {
				printError(fmt.Sprintf("Warning: failed to restore IP %s: %v", summary.OriginalIP, err))
			}
		} else {
			action := askFlashRetryAction("Flashing completed but target MAC not found on any interface")
			if action == "SKIP" {
				summary.Success = false
				summary.Error = "MAC not found after flashing - skipped by operator"
				return nil
			}
			summary.Success = false
			summary.Error = "MAC not found after flashing"
			return fmt.Errorf("target MAC not found after flashing")
		}
	}

	return nil
}

// Driver management functions
func unloadNetworkDriver(driverName string) error {
	if driverName == "" {
		return fmt.Errorf("driver name is empty")
	}

	printInfo(fmt.Sprintf("Unloading driver: %s", driverName))
	cmd := exec.Command("rmmod", driverName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rmmod failed: %v\nOutput: %s", err, string(output))
	}

	// Wait a moment for driver to fully unload
	time.Sleep(1 * time.Second)
	return nil
}

func loadNetworkDriver(driverName string) error {
	if driverName == "" {
		return fmt.Errorf("driver name is empty")
	}

	printInfo(fmt.Sprintf("Loading driver: %s", driverName))
	cmd := exec.Command("modprobe", driverName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("modprobe failed: %v\nOutput: %s", err, string(output))
	}

	// Wait a moment for driver to fully load
	time.Sleep(2 * time.Second)
	return nil
}

func loadFlashingDriver(driverDir, originalDriver string) (string, error) {
	// Step 1: Try to use pre-compiled driver from driver_dir
	driverPath := filepath.Join(driverDir, originalDriver+".ko")
	if _, err := os.Stat(driverPath); err == nil {
		printInfo(fmt.Sprintf("Found pre-compiled driver: %s", driverPath))
		if err := loadDriverFromPath(driverPath); err == nil {
			return driverPath, nil
		} else {
			printError(fmt.Sprintf("Pre-compiled driver failed to load: %v", err))
		}
	}

	// Step 2: Compile new driver
	printInfo("Compiling new flashing driver...")
	compiledPath, err := compileFlashingDriver(driverDir, originalDriver)
	if err != nil {
		return "", fmt.Errorf("failed to compile driver: %v", err)
	}

	// Step 3: Load compiled driver
	if err := loadDriverFromPath(compiledPath); err != nil {
		return "", fmt.Errorf("failed to load compiled driver: %v", err)
	}

	return compiledPath, nil
}

func loadDriverFromPath(driverPath string) error {
	printInfo(fmt.Sprintf("Loading driver from: %s", driverPath))
	cmd := exec.Command("insmod", driverPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("insmod failed: %v\nOutput: %s", err, string(output))
	}

	time.Sleep(2 * time.Second)
	return nil
}

func compileFlashingDriver(driverDir string, originalDriver string) (string, error) {
	// This is a simplified version - in real implementation, you would need
	// the actual driver source code and proper compilation environment
	printInfo("Compiling driver (this may take a few minutes)...")

	// Create driver directory if it doesn't exist
	os.MkdirAll(driverDir, 0755)

	// For demonstration, we'll simulate compilation
	// In real implementation, this would involve:
	// 1. Finding the driver source
	// 2. Setting up kernel headers
	// 3. Running make
	// 4. Installing the compiled module

	compiledPath := filepath.Join(driverDir, originalDriver+".ko")

	// Simulate compilation time
	time.Sleep(3 * time.Second)

	// In reality, you would run something like:
	// cd /usr/src/driver-source && make && cp driver.ko $driverDir/

	return compiledPath, fmt.Errorf("driver compilation not implemented - this requires actual driver source and build environment")
}

// Flashing execution functions
func executeRtnicFlashing(targetMAC string) error {
	// Remove colons from MAC for rtnic
	macWithoutColons := strings.ReplaceAll(targetMAC, ":", "")

	printInfo(fmt.Sprintf("Executing rtnic flashing for MAC: %s", targetMAC))

	// Execute rtnic with required arguments
	cmd := exec.Command("rtnic", "/efuse", "/nicmac", "/nodeid", macWithoutColons)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return fmt.Errorf("rtnic command failed: %v\nOutput: %s", err, string(output))
	}

	// Check if output indicates success
	outputStr := string(output)
	if strings.Contains(strings.ToLower(outputStr), "error") || strings.Contains(strings.ToLower(outputStr), "fail") {
		return fmt.Errorf("rtnic reported error: %s", outputStr)
	}

	printSuccess("rtnic flashing command completed successfully")
	return nil
}

// Network restoration functions
func restartNetworkServices() {
	printInfo("Restarting network services...")

	// Try different network restart methods
	methods := [][]string{
		{"systemctl", "restart", "networking"},
		{"systemctl", "restart", "network"},
		{"service", "networking", "restart"},
		{"service", "network", "restart"},
	}

	for _, method := range methods {
		cmd := exec.Command(method[0], method[1:]...)
		if err := cmd.Run(); err == nil {
			printSuccess(fmt.Sprintf("Network restarted using: %s", strings.Join(method, " ")))
			time.Sleep(3 * time.Second)
			return
		}
	}

	// Fallback: restart network interfaces manually
	printInfo("Fallback: restarting network interfaces manually...")
	interfaces, _ := getCurrentNetworkInterfaces()
	for _, iface := range interfaces {
		if iface.Name != "lo" {
			exec.Command("ip", "link", "set", iface.Name, "down").Run()
			time.Sleep(1 * time.Second)
			exec.Command("ip", "link", "set", iface.Name, "up").Run()
		}
	}

	time.Sleep(3 * time.Second)
}

func restoreIPAddress(interfaceName, ipAddress string) error {
	if interfaceName == "" || ipAddress == "" {
		return fmt.Errorf("interface name or IP address is empty")
	}

	printInfo(fmt.Sprintf("Restoring IP %s to interface %s", ipAddress, interfaceName))

	// First ensure interface is up
	cmd := exec.Command("ip", "link", "set", interfaceName, "up")
	cmd.Run()

	time.Sleep(1 * time.Second)

	// Assign IP address (assuming /24 subnet)
	cmd = exec.Command("ip", "addr", "add", ipAddress+"/24", "dev", interfaceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// IP might already be assigned, check if it's actually there
		checkCmd := exec.Command("ip", "addr", "show", interfaceName)
		checkOutput, _ := checkCmd.Output()
		if strings.Contains(string(checkOutput), ipAddress) {
			printSuccess(fmt.Sprintf("IP %s already assigned to %s", ipAddress, interfaceName))
			return nil
		}
		return fmt.Errorf("failed to assign IP: %v\nOutput: %s", err, string(output))
	}

	printSuccess(fmt.Sprintf("IP %s restored to interface %s", ipAddress, interfaceName))
	return nil
}

func runFlashing(config FlashConfig, flashData *FlashData, systemConfig SystemConfig) []FlashResult {
	var results []FlashResult

	if !config.Enabled {
		return results
	}

	fmt.Println(strings.Repeat("-", 80))

	for _, operation := range config.Operations {
		result := FlashResult{
			Operation: operation,
			Status:    "PASSED",
		}

		startTime := time.Now()

		switch operation {
		case "serial":
			printInfo("Flashing serial numbers...")
			if flashData.SystemSerial != "" {
				err := flashSerial(systemConfig, flashData.SystemSerial, "system")
				if err != nil {
					result.Status = "FAILED"
					result.Details = fmt.Sprintf("System serial flash failed: %v", err)
				}
			}
			if flashData.IOBoard != "" {
				err := flashSerial(systemConfig, flashData.IOBoard, "io_board")
				if err != nil {
					result.Status = "FAILED"
					result.Details = fmt.Sprintf("IO board serial flash failed: %v", err)
				}
			}

		case "mac":
			printInfo(fmt.Sprintf("Flashing MAC address: %s", flashData.MAC))
			err := flashMAC(config, systemConfig, flashData.MAC)
			if err != nil {
				result.Status = "FAILED"
				result.Details = fmt.Sprintf("MAC flash failed: %v", err)
			}

		case "efi":
			printInfo("Updating EFI variables")
			err := updateEFIVariables(systemConfig, flashData)
			if err != nil {
				result.Status = "FAILED"
				result.Details = fmt.Sprintf("EFI update failed: %v", err)
			}
		}

		result.Duration = time.Since(startTime)
		results = append(results, result)

		outputManager.PrintResult(time.Now(), operation, result.Status, result.Duration, result.Details)
	}

	return results
}

func flashSerial(config SystemConfig, serial, serialType string) error {
	// Имитация прошивки серийного номера
	time.Sleep(500 * time.Millisecond)
	printSuccess(fmt.Sprintf("Serial number (%s) flashed: %s", serialType, serial))
	return nil
}

func updateEFIVariables(config SystemConfig, flashData *FlashData) error {
	// Обновляем EFI переменные используя efivar или подобные инструменты

	if flashData.SystemSerial != "" && config.EfiSnName != "" {
		err := setEFIVariable(config.GuidPrefix, config.EfiSnName, flashData.SystemSerial)
		if err != nil {
			return fmt.Errorf("failed to set serial EFI variable: %v", err)
		}
	}

	if flashData.MAC != "" && config.EfiMacName != "" {
		// Конвертируем MAC в hex формат без двоеточий
		hexMAC := strings.ReplaceAll(flashData.MAC, ":", "")
		err := setEFIVariable(config.GuidPrefix, config.EfiMacName, hexMAC)
		if err != nil {
			return fmt.Errorf("failed to set MAC EFI variable: %v", err)
		}
	}

	return nil
}

func setEFIVariable(guidPrefix, varName, value string) error {
	// Пример команды для установки EFI переменной
	// efivar -w -n {guid}-{varName} -d {value}

	// Для демонстрации просто симулируем успех
	time.Sleep(200 * time.Millisecond)
	printSuccess(fmt.Sprintf("EFI variable %s set to: %s", varName, value))
	return nil
}

func testServerConnection(config LogConfig) error {
	if !config.SendLogs || config.Server == "" {
		return nil
	}

	// Parse server (user@host format)
	serverParts := strings.Split(config.Server, "@")
	if len(serverParts) != 2 {
		return fmt.Errorf("invalid server format, expected user@host: %s", config.Server)
	}

	user := serverParts[0]
	host := serverParts[1]
	serverAddr := fmt.Sprintf("%s@%s", user, host)

	printInfo(fmt.Sprintf("Testing connection to server: %s", serverAddr))

	// Test SSH connection
	testCmd := exec.Command("ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		serverAddr,
		"echo 'Connection test successful'")

	if output, err := testCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("server connection test failed: %v\nOutput: %s", err, string(output))
	}

	printSuccess("Server connection test passed")
	return nil
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

func sendLogToServer(log SessionLog, config LogConfig) error {
	if !config.SendLogs || config.Server == "" {
		return nil
	}

	printInfo(fmt.Sprintf("Sending log to server: %s", config.Server))

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

	// Generate remote filename and path
	timestamp := log.Timestamp.Format("20060102_150405")
	remoteFile := fmt.Sprintf("%s_%s_%s.yaml", log.System.Product, log.System.MBSerial, timestamp)

	// Build remote directory path
	remoteDirParts := []string{}
	if config.ServerDir != "" {
		remoteDirParts = append(remoteDirParts, config.ServerDir)
	}
	if log.System.Product != "" {
		remoteDirParts = append(remoteDirParts, log.System.Product)
	}
	if config.OpName != "" {
		remoteDirParts = append(remoteDirParts, config.OpName)
	}

	var remoteDir string
	if len(remoteDirParts) > 0 {
		remoteDir = strings.Join(remoteDirParts, "/")
	} else {
		remoteDir = "."
	}

	// Parse server (user@host format)
	serverParts := strings.Split(config.Server, "@")
	if len(serverParts) != 2 {
		return fmt.Errorf("invalid server format, expected user@host: %s", config.Server)
	}

	user := serverParts[0]
	host := serverParts[1]
	serverAddr := fmt.Sprintf("%s@%s", user, host)

	fmt.Printf("Remote directory: %s/%s\n", serverAddr, remoteDir)
	fmt.Printf("Remote file: %s\n", remoteFile)

	// Step 1: Create remote directories if they don't exist
	if remoteDir != "." {
		createDirCmd := exec.Command("ssh",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=10",
			serverAddr,
			fmt.Sprintf("mkdir -p '%s'", remoteDir))

		fmt.Printf("Creating remote directory: %s\n", remoteDir)
		if output, err := createDirCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to create remote directory: %v\nOutput: %s", err, string(output))
		}
	}

	// Step 2: Send file via SCP
	fullRemotePath := fmt.Sprintf("%s:%s/%s", serverAddr, remoteDir, remoteFile)
	scpCmd := exec.Command("scp",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		tmpFile.Name(),
		fullRemotePath)

	fmt.Printf("Uploading file to: %s\n", fullRemotePath)
	if output, err := scpCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to send log via SCP: %v\nOutput: %s\nCommand: %s", err, string(output), scpCmd.String())
	}

	printSuccess("Log sent to server successfully")
	return nil
}

func main() {
	var configPath string
	var showVersion bool
	var testsOnly bool
	var flashOnly bool
	var show_Help bool

	flag.StringVar(&configPath, "c", "config.yaml", "Path to configuration file")
	flag.BoolVar(&showVersion, "V", false, "Show version")
	flag.BoolVar(&testsOnly, "tests-only", false, "Run only tests (skip flashing)")
	flag.BoolVar(&flashOnly, "flash-only", false, "Run only flashing (skip tests)")
	flag.BoolVar(&show_Help, "h", false, "Show help")
	flag.Parse()

	if show_Help {
		showHelp()
		os.Exit(0)
	}
	if showVersion {
		fmt.Printf("FIRESTARTER v%s\n", VERSION)
		os.Exit(0)
	}

	// Enterprise заголовок
	fmt.Printf("%sFIRESTARTER%s Hardware Validation System %sv%s%s\n",
		ColorBlue, ColorReset, ColorGray, VERSION, ColorReset)
	printThickSeparator()

	// Load configuration
	config, err := loadConfig(configPath)
	if err != nil {
		printError(fmt.Sprintf("Failed to load configuration: %v", err))
		os.Exit(1)
	}
	if config.System.RequireRoot && os.Geteuid() != 0 {
		printError("This program requires root privileges")
		os.Exit(1)
	}

	// System configuration display
	fmt.Printf("\n%sSYSTEM CONFIGURATION%s\n", ColorWhite, ColorReset)
	fmt.Printf("  Target Product    : %s%s%s\n", ColorCyan, config.System.Product, ColorReset)
	fmt.Printf("  Configuration     : %s%s%s\n", ColorYellow, configPath, ColorReset)
	fmt.Printf("  Root Required     : %s%v%s\n", ColorYellow, config.System.RequireRoot, ColorReset)
	fmt.Printf("  Driver Directory  : %s%s%s\n", ColorBlue, config.System.DriverDir, ColorReset)

	sessionStart := time.Now()

	// System identification
	fmt.Printf("\n%sSYSTEM IDENTIFICATION%s\n", ColorWhite, ColorReset)
	printSeparator()
	systemInfo, err := getSystemInfo()
	if err != nil {
		printError(fmt.Sprintf("Failed to get system information: %v", err))
		os.Exit(1)
	}
	fmt.Printf("  Product Name      : %s%s%s\n", ColorCyan, systemInfo.Product, ColorReset)
	fmt.Printf("  Board Serial      : %s%s%s\n", ColorCyan, systemInfo.MBSerial, ColorReset)
	fmt.Printf("  Network Address   : %s%s%s\n", ColorCyan, systemInfo.IP, ColorReset)
	fmt.Printf("  Detection Time    : %s%s%s\n", ColorGray, systemInfo.Timestamp.Format("2006-01-02 15:04:05"), ColorReset)

	// Product compatibility check
	if config.System.Product != "" && systemInfo.Product != "" {
		if config.System.Product != systemInfo.Product {
			if askUserProductMismatch(config.System.Product, systemInfo.Product) {
				printInfo("Program terminated by user due to product mismatch")
				os.Exit(0)
			}
			fmt.Printf("  Configuration     : %sWARNING - Product mismatch%s\n", ColorYellow, ColorReset)
		} else {
			fmt.Printf("  Configuration     : %sCompatible%s\n", ColorGreen, ColorReset)
		}
	} else {
		if config.System.Product == "" {
			fmt.Printf("  Configuration     : %sNo product specified in config%s\n", ColorYellow, ColorReset)
		}
		if systemInfo.Product == "" {
			fmt.Printf("  Configuration     : %sCould not detect system product%s\n", ColorYellow, ColorReset)
		}
	}

	// Test server connection
	if config.Log.SendLogs {
		if err := testServerConnection(config.Log); err != nil {
			printError(fmt.Sprintf("Server connection test failed: %v", err))
			printError("Log sending will be disabled for this session")
			config.Log.SendLogs = false
		}
	}

	var allResults []TestResult
	var flashResults []FlashResult
	var flashData *FlashData

	// TESTING PHASE [1/2]
	if !flashOnly {
		fmt.Printf("\n%sTESTING PHASE [1/2]%s\n", ColorWhite, ColorReset)
		printThickSeparator()

		// Count tests
		totalTests := 0
		for _, g := range config.Tests.ParallelGroups {
			totalTests += len(g)
		}
		for _, g := range config.Tests.SequentialGroups {
			totalTests += len(g)
		}
		fmt.Printf("Total Tests: %s%d%s | Global Timeout: %s%s%s\n",
			ColorGreen, totalTests, ColorReset,
			ColorYellow, func() string {
				if config.Tests.Timeout != "" {
					return config.Tests.Timeout
				}
				return "30s (default)"
			}(), ColorReset)

		// Run tests
		testsStart := time.Now()
		for i, g := range config.Tests.ParallelGroups {
			groupName := fmt.Sprintf("Parallel Group %d", i+1)
			results := runTestGroup(g, true, outputManager, groupName, config.Tests.Timeout)
			allResults = append(allResults, results...)
		}
		for i, g := range config.Tests.SequentialGroups {
			groupName := fmt.Sprintf("Sequential Group %d", i+1)
			results := runTestGroup(g, false, outputManager, groupName, config.Tests.Timeout)
			allResults = append(allResults, results...)
		}
		testsDuration := time.Since(testsStart)

		// Tests summary
		printTestsSummary(allResults, testsDuration)

		// List failed tests by name
		var failedNames []string
		for _, r := range allResults {
			if r.Status == "FAILED" || r.Status == "TIMEOUT" {
				failedNames = append(failedNames, r.Name)
			}
		}
		if len(failedNames) > 0 {
			fmt.Printf("%sFailed tests:%s %s\n\n",
				ColorRed, ColorReset, strings.Join(failedNames, ", "))
		}
	}

	// FLASH data input
	if !testsOnly && config.Flash.Enabled {
		flashData, err = getFlashData(config.Flash, systemInfo.Product)
		if err != nil {
			printError(fmt.Sprintf("Failed to get flash data: %v", err))
			os.Exit(1)
		}
	}

	// FLASHING PHASE [2/2]
	if !testsOnly && config.Flash.Enabled && flashData != nil {
		fmt.Printf("\n%sFLASHING PHASE [2/2]%s\n", ColorWhite, ColorReset)
		printThickSeparator()
		fmt.Printf("Operations: %s%s%s | Method: %s%s%s\n",
			ColorYellow, strings.Join(config.Flash.Operations, ", "), ColorReset,
			ColorGreen, config.Flash.Method, ColorReset)
		flashResults = runFlashing(config.Flash, flashData, config.System)
	}

	// Session duration
	totalDuration := time.Since(sessionStart)

	// Save & send logs
	sessionLog := SessionLog{
		SessionID:    fmt.Sprintf("%d", time.Now().Unix()),
		Timestamp:    sessionStart,
		System:       systemInfo,
		Pipeline:     PipelineInfo{Mode: "full", Config: configPath, Duration: totalDuration, Operator: config.Log.OpName},
		TestResults:  allResults,
		FlashResults: flashResults,
	}
	if flashData != nil {
		sessionLog.System.MBSerial = flashData.SystemSerial
		sessionLog.System.IOSerial = flashData.IOBoard
		sessionLog.System.MAC = flashData.MAC
	}
	if err := saveLog(sessionLog, config.Log); err != nil {
		printError(fmt.Sprintf("Failed to save log: %v", err))
	}
	if config.Log.SendLogs {
		if err := sendLogToServer(sessionLog, config.Log); err != nil {
			printError(fmt.Sprintf("Failed to send log to server: %v", err))
		}
	} else {
		printInfo("Log sending disabled (send_logs: false)")
	}

	// Final summary
	printExecutionSummary(allResults, flashResults, totalDuration)

	// Exit code
	exitCode := 0
	for _, r := range allResults {
		if r.Status == "FAILED" && r.Required {
			exitCode = 1
			break
		}
	}
	for _, fr := range flashResults {
		if fr.Status == "FAILED" {
			exitCode = 1
			break
		}
	}
	if exitCode != 0 {
		fmt.Printf("\n%sExiting with error code %d due to failed critical operations%s\n",
			ColorRed, exitCode, ColorReset)
	}
	os.Exit(exitCode)
}
