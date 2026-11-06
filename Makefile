BINARY := crom-ar
SRC_HD ?= ../crompressor/bin/crompressor

.PHONY: build pull-bin test vet clean install

build: pull-bin
	go build -o bin/$(BINARY) ./cmd/crom-ar

# Puxa o binário do crompressor (repositório irmão ../crompressor por padrão; sobrescreva com SRC_HD=/caminho)
pull-bin:
	@mkdir -p bin; \
	if [ -f "$(SRC_HD)" ]; then \
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
