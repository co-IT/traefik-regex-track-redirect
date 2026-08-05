.PHONY: fmt test vet integration integration-down

fmt:
	gofmt -w .

test:
	go test -race ./...

vet:
	go vet ./...

integration:
	docker compose -f integration/docker-compose.yml up -d
	bash integration/test.sh

integration-down:
	docker compose -f integration/docker-compose.yml down -v
