run:
	go run . --mode=server --addr=localhost:8080

run-client:
	go run . --mode=client --addr=localhost:8080

build:
	go build -o adhd ./main.go
