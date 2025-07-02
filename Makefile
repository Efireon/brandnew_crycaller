build_gpu_test:
	(cd source/gpu_test && go mod tidy)
	(cd source/gpu_test && go build -o gpu_test main.go)
	mv source/gpu_test/gpu_test bin/

build_fan_test:
	(cd source/fan_test && go mod tidy)
	(cd source/fan_test && go build -o fan_test main.go)
	mv source/fan_test/fan_test bin/

build_cpu_test:
	(cd source/cpu_test && go mod tidy)
	(cd source/cpu_test && go build -o cpu_test main.go)
	mv source/cpu_test/cpu_test bin/

build_ram_test:
	(cd source/ram_test && go mod tidy)
	(cd source/ram_test && go build -o ram_test main.go)
	mv source/ram_test/ram_test bin/

build_firestarter:
	(cd source/firestarter && go mod tidy)
	(cd source/firestarter && go build -o firestarter main.go)
	mv source/firestarter/firestarter bin/

build_disk_test:
	(cd source/disk_test && go mod tidy)
	(cd source/disk_test && go build -o disk_test main.go)
	mv source/disk_test/disk_test bin/

build_network_test:
	(cd source/network_test && go mod tidy)
	(cd source/network_test && go build -o network_test main.go)
	mv source/network_test/network_test bin/

build_power_test:
	(cd source/power_test && go mod tidy)
	(cd source/power_test && go build -o power_test main.go)
	mv source/power_test/power_test bin/


build_all: build_fan_test build_gpu_test build_cpu_test build_ram_test build_firestarter build_disk_test build_network_test build_power_test