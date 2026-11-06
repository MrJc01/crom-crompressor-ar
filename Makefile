BINARY := crom-ar
CROM_SRC ?= ../crompressor
SRC_HD ?= $(CROM_SRC)/bin/crompressor

.PHONY: build pull-bin test vet clean install

build: pull-bin
	go build -o bin/$(BINARY) ./cmd/crom-ar

# Motor crompressor: compilado do FONTE (CROM_SRC). O binário em ../crompressor/bin
# pode estar mais velho que o código (aconteceu em 30/10). Sem o fonte, copia SRC_HD.
pull-bin:
	@mkdir -p bin; \
	if [ -f "$(CROM_SRC)/cmd/crompressor/main.go" ]; then \
		(cd "$(CROM_SRC)" && go build -o "$(abspath bin/crompressor)" ./cmd/crompressor) && \
		echo "crompressor compilado do fonte $(CROM_SRC) ($$(git -C "$(CROM_SRC)" log -1 --format=%h 2>/dev/null))"; \
	elif [ -f "$(SRC_HD)" ]; then \
		if cmp -s "$(SRC_HD)" bin/crompressor; then echo "bin/crompressor ok (igual a $(SRC_HD))"; \
		else cp "$(SRC_HD)" bin/crompressor && chmod +x bin/crompressor && echo "crompressor atualizado de $(SRC_HD)"; fi; \
	elif [ -f bin/crompressor ]; then echo "aviso: $(SRC_HD) não encontrado; usando bin/crompressor existente"; \
	else echo "crompressor não encontrado em $(SRC_HD) — defina SRC_HD=/caminho make build"; exit 1; fi

test:
	go test ./...

test-race:
	go test -race ./...
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin/crom-ar

install: build
	./bin/crom-ar install
