build_gpu_test:
	(cd source/gpu_test && go mod tidy)
	go build -o bin/gpu_test source/gpu_test/main.go

build_fan_test:
	(cd source/fan_test && go mod tidy)
	go build -o bin/fan_test source/fan_test/main.go
