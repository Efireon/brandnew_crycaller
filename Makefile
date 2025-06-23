build_gpu_test:
	(cd source/gpu_test && go mod tidy)
	go build -o bin/gpu_test source/gpu_test/main.go

build_fan_test:
	(cd source/fan_test && go mod tidy)
	(cd source/fan_test && go build -o fan_test main.go)
	mv source/fan_test/fan_test bin/
