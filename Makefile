TEST_DATABASE_URL ?= postgres://postgres:postgres@localhost:5432/ledger?sslmode=disable

.PHONY: run test up down
up:      ## start Postgres + ledgerd
	docker compose up --build
down:
	docker compose down -v
test:    ## needs Postgres (e.g. `docker compose up -d postgres`)
	TEST_DATABASE_URL=$(TEST_DATABASE_URL) go test -p 1 -race -count=1 ./...
run:
	go run ./cmd/ledgerd
