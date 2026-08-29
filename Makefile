BINARY := crom-ar
SRC_HD ?= /media/j/j0/GitHub/crom/crompressor/crompressor

.PHONY: build pull-bin test vet clean install

build: pull-bin
	go build -o bin/$(BINARY) ./cmd/crom-ar

# Puxa o binário do crompressor (HD externo por padrão; sobrescreva com SRC_HD=/caminho)
pull-bin:
	@if [ -f bin/crompressor ]; then echo "bin/crompressor ok"; \
	elif [ -f "$(SRC_HD)" ]; then cp "$(SRC_HD)" bin/crompressor && chmod +x bin/crompressor && echo "crompressor puxado de $(SRC_HD)"; \
	else echo "crompressor não encontrado em $(SRC_HD) — defina SRC_HD=/caminho make build"; exit 1; fi

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin/crom-ar

install: build
	./bin/crom-ar install
