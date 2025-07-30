package main

import (
	pr "bmc_test/internal/print"
	"fmt"
	"os"
	"os/exec"
)

var VERSION = "1.0.0"

var bmcPaths = []string{
	"/dev/ipmi0",
	"/dev/ipmi1",
	"/dev/ipmi2",
	"/dev/ipmi/0",
	"/dev/ipmidev/0",
}

var bmcModules = []string{
	"ipmi_si",
	"ipmi_ssif",
	"acpi_ipmi",
	"ipmi_devintf",
	"ipmi_msghandler",
}

type ipmiState struct {
	ipmiDevPath  string
	ipmiDevState bool
	ipmiModules  []string
	ipmiModState bool
}

func isExist(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isDriverLoaded() ([]string, bool) {

	var modules []string

	for _, module := range bmcModules {
		if isExist(fmt.Sprint("/sys/module/", module)) {
			modules = append(modules, module)
		}
	}

	if modules != nil {
		return modules, true
	} else {
		return []string{}, false
	}
}

func devicePresent() (string, bool) {
	for _, path := range bmcPaths {
		if isExist(path) {
			return path, true
		}
	}
	return "", false
}

func ipmiCheck() ipmiState {
	state := ipmiState{}

	// Проверяем наличие устройства в /dev
	pr.Info("Searching for IPMI device...")
	state.ipmiDevPath, state.ipmiDevState = devicePresent()

	if state.ipmiDevState {
		pr.Success(fmt.Sprintf("Succesfuly found: %s", state.ipmiDevPath))
	} else {
		pr.Fail("IPMI not found")
	}

	pr.Debug("")

	// Проверяем наличие модулей в /sys/modules
	pr.Info("Searching for IPMI modules...")
	state.ipmiModules, state.ipmiModState = isDriverLoaded()

	if state.ipmiModState {
		pr.Success("Succesfuly found modules:")
		for i, module := range state.ipmiModules {
			pr.Info(fmt.Sprintf("%d. %s", i, module))
		}
	} else {
		pr.Fail("IPMI modules not found")
	}

	return state
}

func unloadModules(modules []string) ([]string, []string) {
	var fail, success []string

	for _, module := range modules {
		cmd := exec.Command("rmmod", module)

		err := cmd.Run()

		if err != nil {
			fail = append(fail, module)
		} else {
			success = append(success, module)
		}
	}

	return fail, success
}

func loadModules() ([]string, []string) {
	var fail, success []string

	for _, module := range bmcModules {
		cmd := exec.Command("modprobe", module)

		err := cmd.Run()

		if err != nil {
			fail = append(fail, module)
		} else {
			success = append(success, module)
		}
	}

	return fail, success
}

func reloadModules(modules []string) bool {

	unloadFail, unloadSuccess := unloadModules(modules)

	loadFail, loadSuccess := loadModules()

	pr.Info("\nUnloading modules...")

	if unloadFail != nil {
		pr.Fail("Failed to unload:")

		for i, f := range unloadFail {
			pr.Warning(fmt.Sprintf("%d: %s", i, f))
		}
	}

	fmt.Println("")

	if unloadSuccess != nil {
		pr.Success("Succesfully unloaded:")

		for i, s := range unloadSuccess {
			pr.Info(fmt.Sprintf("%d: %s", i, s))
		}
	}

	pr.Info("\nLoading modules...")

	if loadFail != nil {
		pr.Fail("Failed to load:")

		for i, f := range loadFail {
			pr.Warning(fmt.Sprintf("%d: %s", i, f))
		}
	}

	if loadSuccess != nil {
		pr.Success("Succesfully loaded:")

		for i, s := range loadSuccess {
			pr.Info(fmt.Sprintf("%d: %s", i, s))
		}
	}

	if (unloadFail == nil && unloadSuccess != nil) && (loadFail == nil && loadSuccess != nil) {
		return true
	} else {
		return false
	}

}

/*
	unload := exec.Command("rmmod", modules...)

	err := unload.Run()

	if err != nil {
		return false
	} else {
		return true
	}
*/

func main() {
	switch bmc := ipmiCheck(); {
	case bmc.ipmiDevState && bmc.ipmiModState:
		pr.Success("\nOK!\nThere is nothing to do. Device & Modules are already up")
		pr.Info(fmt.Sprintf("Device: %s", bmc.ipmiDevPath))

		for i, module := range bmc.ipmiModules {
			pr.Info(fmt.Sprintf("%d: %s", i, module))
		}

		os.Exit(0)
	default:
		if reloadModules(bmc.ipmiModules) {
			pr.Success("Unload successful")
			switch bmc = ipmiCheck(); {
			case bmc.ipmiDevState && bmc.ipmiModState:
				pr.Success("\nOK!\nDevice & Modules are up")
				pr.Info(fmt.Sprintf("Device: %s", bmc.ipmiDevPath))

				for i, module := range bmc.ipmiModules {
					pr.Info(fmt.Sprintf("%d: %s", i, module))
				}

				os.Exit(0)
			default:
				pr.Fail("\nFailed to revive BMC.")
				os.Exit(1)
			}
		} else {
			pr.Fail("\nFailed to reload modules.")
			os.Exit(1)
		}
	}
}

/*
	if path, state := devicePresent(); state {
		fmt.Printf("ipmi device found: %s\n", path)
		os.Exit(0)
	} else {
		fmt.Println("ipmi device not found")
		if drv, state := isDriverLoaded(); state {
			fmt.Println("Found: ", drv)
		} else {
			fmt.Println("Can't find ipmi module")
		}
	}
*/
