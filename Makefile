build_gpu_test:
	(cd source/gpu_test && go mod tidy)
	go build -o bin/gpu_test source/gpu_test/main.go