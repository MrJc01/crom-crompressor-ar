# Conceitos do sistema CROM: o que o usuário explicou e o que aprendemos

Este arquivo junta a visão do usuário (nas palavras dele) e o que o código mostra hoje. Quando os dois
divergem, a visão do usuário é o destino e o código é o ponto de partida. Atualize a cada conversa.

---

## 1. A visão do usuário

- "O crompressor é só a base do nosso sistema cromar." O motor é a origem; o crom-ar é o produto em cima dele.
- Arquivos novos ou parecidos **compartilham o mesmo bloco** e guardam só **"o raio que o modifica para o
  original"** (a diferença). É o que o delta do depósito faz hoje.
- **O `.cromar` e o depósito ficam os dois locais**, no PC do usuário. Não é só um arquivo de exportação.
- **Blocos nunca passam na rede.** Se for preciso, só passam os deltas que montam o bloco do outro lado.
- **A ideia de "cromprimir" era exatamente ter o retreino automático com os dados quando necessário.** O
  cérebro deve acompanhar os dados, sem o usuário rodar `train` na mão.
- Pacotes que não sincronizam entre si por terem cérebros diferentes é **"um problema colossal"**.
- Ideia para depois: o cofre repartido entre pessoas na rede, cada uma guardando partes, mas acessível a
  todos (seção 9). Talvez caiba em outro projeto.

---

## 2. Glossário curto

| Termo | O que é |
|---|---|
| Bloco | Pedaço de um arquivo (no depósito, uns 16 KB, cortado pelo conteúdo com FastCDC) |
| Hash (SHA-256) | "Impressão digital" de 32 bytes de um bloco. Mesmo conteúdo → mesmo hash, em qualquer máquina |
| Receita | Lista de hashes, em ordem, que monta um arquivo |
| Frame | Grupo de blocos de ~1 MB comprimido junto com zstd, gravado no disco |
| Delta | Bloco guardado como "hash do bloco base + diferença" |
| Base | Bloco já guardado que serve de referência para um delta |
| Features / `feats.log` | Catálogo de impressões aproximadas (minhash) usado para **achar** a base parecida; não guarda dado |
| Cérebro (`.cromdb`) | Dicionário do motor: blocos frequentes numerados 0, 1, 2… |
| `.crom` | Arquivo do motor: números do cérebro + trechos novos comprimidos |
| `.cromar` | Pacote do crom-ar: zip com `meta.json` + cérebro + `content.crom` |
| GC | Faxina que apaga blocos que nenhuma receita usa mais |

---

## 3. Como o depósito funciona (crom-ar, branch `deposito-blocos`)

```
arquivo ──receita──▶ [hash1, hash2, hash3 …]                  monta o ARQUIVO
hash ──índice──▶ bloco inteiro dentro de um frame zstd          dado bruto
            ou ▶ delta: hash da base + diferença                monta o BLOCO
```

### "Tudo o que entra vira dicionário, sem treino"

No motor, alguém precisa **escolher** quais blocos entram no cérebro (o `train` conta os mais frequentes e
guarda até `-s` entradas). Isso é "treino".

No depósito não existe essa escolha: **todo bloco que entra fica guardado e passa a servir de referência**
para os próximos. Um arquivo novo é cortado em blocos e cada hash é procurado no índice:

- se já existe, a receita só aponta para ele (custo zero);
- se não existe, mas há um parecido, vira delta;
- se não há nada parecido, é guardado inteiro.

O "dicionário" do depósito é o próprio cofre, e ele "se retreina" sozinho a cada `add`. É o retreino
automático que o usuário quer, só que sem treino de verdade: é acumulação.

### "SHA-256 não depende de ordem nem de treino"

O motor identifica um bloco pelo **número dele no cérebro** (entrada 1234). Esse número só faz sentido
naquele cérebro: se o cérebro for retreinado, a entrada 1234 pode virar outro bloco, e os `.crom` antigos
apontam para o lugar errado.

O depósito identifica um bloco pelo **SHA-256 do conteúdo**. O hash sai do próprio bloco: é o mesmo em
qualquer máquina, em qualquer data, em qualquer ordem de chegada. Não há nada para "combinar" antes, e
nada quebra quando o cofre cresce ou diminui. É o mesmo princípio do git e do IPFS.

### "Delta contra um bloco escolhido"

No motor, os trechos novos são comprimidos com zstd usando **o cérebro inteiro** (até 32 MB) como
dicionário: o zstd procura repetições dentro desses 32 MB.

No depósito isso não dá: o cofre tem gigabytes, e nenhum zstd usa gigabytes como dicionário (seria lento e
gastaria a RAM toda). Então, para cada bloco novo, o depósito:

1. calcula 3 impressões aproximadas (minhash) do bloco;
2. procura no `feats.log` um bloco guardado com alguma impressão igual (o "parecido");
3. comprime o bloco novo com zstd usando **só aquele bloco** como dicionário (16 KB, rápido);
4. aceita se o resultado for menor que metade do bloco e conferir byte a byte.

É a mesma ideia do motor, em escala de um bloco.

### Números (10 GB reais, 9 projetos)

| | tamanho |
|---|---|
| Cofre antes do delta | 3250,6 MB |
| Cofre com delta | **3161,2 MB** |
| tar + zstd -19 --long | 3207,6 MB |
| 7z, um por projeto | 3017,4 MB |
| zip | 4628,6 MB |

### O que ainda falta no depósito

- `feats.log` não é limpo no GC (só cresce, 60 bytes por bloco).
- **Frames frios:** recomprimir em segundo plano, com nível mais forte, os frames que ninguém mexe. O `add`
  continua rápido; o leitor vê o mesmo conteúdo. O motor já tem o encoder zstd 19 em C
  (`internal/delta/compress_max_cgo.go`). LZMA comprime mais, mas lê mais devagar: só para frame frio.

---

## 4. Como o motor funciona (crompressor)

- `train`: conta os blocos de um corpus e guarda os mais frequentes no cérebro, numerados.
- `pack`: acha os blocos do cérebro em qualquer posição do arquivo (hash rolante). Bloco idêntico → só o
  número. O resto → zstd com o cérebro como dicionário. Opcional: resíduo XOR para bloco aproximado (a busca
  aproximada só compara blocos com os mesmos 2 primeiros bytes — pendência do próprio motor).
- `unpack`: remonta e confere o SHA-256; se não bater, não grava nada.
- O ganho grande é com dados que o cérebro já viu (99,9%). Dados novos ficam perto do zstd puro.
- `train --update`: junta entradas antigas e novas e **escolhe de novo** as melhores. Entradas antigas podem
  sair e os números mudam → os `.crom` antigos deixam de abrir com o cérebro novo. A pendência do motor
  ("testar `train --update` sem invalidar os `.crom` existentes") é exatamente isso.

### Por que motor e depósito são tão diferentes

Não é defeito de um ou de outro: resolvem problemas diferentes.

| | Motor | Depósito |
|---|---|---|
| Problema | mandar dados para **outra máquina** | guardar no **próprio disco** |
| Dicionário | fechado e combinado entre as pontas | aberto, cresce e diminui sozinho |
| Nome do bloco | número no cérebro (pequeno, mas depende do cérebro) | SHA-256 (32 bytes, mas universal) |
| Parecido | zstd com o cérebro inteiro | delta contra um bloco |
| Apagar | não existe | GC |

---

## 5. O `.cromar` hoje

`crom-ar export` extrai os arquivos do depósito, faz um tar, **treina um cérebro só com esse pacote**, roda
o `crompressor pack` e fecha um zip (`meta.json` + cérebro + `content.crom`). É uma cópia gerada na hora e
não fica no cofre. Se o usuário guardar o `.cromar` ao lado do cofre, os dados ocupam espaço duas vezes.

Visão do usuário: `.cromar` e depósito locais, sem duplicar. Caminho provável: o `.cromar` deixa de ser
cópia e vira **uma visão** do cofre — a lista de receitas de um pacote, apontando para os blocos do
depósito — e só vira zip completo quando sai da máquina.

---

## 6. A rede do motor, em palavras simples

1. Uma máquina liga o "servidor" (`crompressor daemon`) apontando para uma pasta com arquivos `.crom`.
2. Outra máquina quer um desses arquivos (`crompressor pull`). Antes de tudo, as duas **provam uma para a
   outra que têm o mesmo cérebro**, sem mostrá-lo: cada uma manda um número aleatório e a outra devolve uma
   "assinatura" calculada com o cérebro. Só quem tem o cérebro consegue calcular.
3. Quem pede recebe a **lista de peças** do arquivo (para cada bloco: número no cérebro + impressão da
   diferença). Compara com a versão antiga que já tem e pede só as peças que faltam.
4. As peças chegam comprimidas com o cérebro como dicionário; o arquivo é remontado e conferido (SHA-256).

Outras peças existem, mas são experimentais: anunciar arquivos para a rede, achar outras máquinas na rede
local ou pela internet, "cérebro federado". Não há relay: se as duas máquinas estão atrás de roteador sem
porta aberta, elas não se conectam.

### O problema colossal

Cada `.cromar` tem **um cérebro próprio**. Dois pacotes, ou duas versões do mesmo pacote, têm cérebros
diferentes: as máquinas não passam da etapa 2 e os números da etapa 3 não batem. E se o cérebro for
retreinado, os `.crom` antigos param de abrir. Com o desenho atual, **retreino automático e sincronização
são incompatíveis**.

### Caminho que resolve os dois

O depósito já tem o que falta: nomes universais (SHA-256) e dicionário que cresce sem quebrar nada.

- **Cérebro que só cresce (append-only):** entradas novas entram no fim; nenhuma sai nem muda de número.
  Retreinar = acrescentar. Os `.crom` antigos continuam valendo. O cérebro de um cofre pode ser o próprio
  conjunto de blocos do depósito.
- **Sincronizar cofre com cofre**, usando o transporte do motor (conexão cifrada, NAT, descoberta):
  1. um lado manda as receitas do que quer compartilhar (listas de hashes);
  2. o outro responde quais hashes não tem;
  3. para cada bloco que falta, segue **um delta contra um bloco que o outro lado já tem** (a regra do
     usuário: blocos nunca passam, só deltas). Se o outro lado não tem nada parecido, o "delta" é o bloco
     comprimido contra nada — a informação nova precisa passar de alguma forma, mas sempre o mínimo.
  4. o receptor monta e confere o SHA-256 de cada bloco e de cada arquivo.

  Não precisa de cérebro igual dos dois lados: precisa só saber o que o outro tem.

### Precisa ser um cérebro que só cresce? (pergunta do usuário, 06/11)

Não. O que precisa é: **o nome de um bloco nunca muda** e **um bloco só sai quando nenhum arquivo o usa**.
"Só cresce" é uma forma de garantir isso quando o nome é um número; com nome por hash, não precisa.

| Jeito | Como | Custo |
|---|---|---|
| Nome por hash (o depósito) | bloco chamado pelo SHA-256; GC apaga só o que ninguém usa | 32 bytes por referência no disco |
| Cérebro que só cresce | entrada nova no fim, nenhuma muda de número | cresce para sempre; a "assinatura" do cérebro muda a cada acréscimo |
| Versões do cérebro | cada `.crom` grava qual versão usou; versões antigas guardadas | guardar todas as versões |
| Tabela de tradução | retreina e grava "número velho → número novo" | uma tabela por retreino |
| Recomprimir tudo | retreina e refaz todos os `.crom` | CPU e disco a cada retreino; as duas pontas da rede juntas |

O que impede hoje no motor: o `train --update` re-escolhe as entradas (números mudam, entradas saem); a
prova de cérebro na rede usa o hash do cérebro inteiro (qualquer acréscimo vira "outro cérebro"); o cérebro
é carregado na RAM (não cresce para gigabytes); e o zstd só usa os 32 MB finais como dicionário.

Por que o motor usa número: é curto (2 a 4 bytes contra 32 do hash), e isso importa na rede. Proposta
híbrida: **no disco, hash** (o depósito é o cérebro, cresce e diminui sem quebrar nada); **na rede, número
combinado na hora** — as duas pontas numeram a lista de hashes que ambas têm e trocam só os números curtos.

---

## 7. Segurança na rede

O que o motor já tem:

- Conexão cifrada e autenticada pelo libp2p (TCP e QUIC), com identidade Ed25519 por máquina (`peer.key`).
- Prova mútua de posse do cérebro (HMAC com número aleatório, presa à identidade de quem prova — não dá
  para repetir nem refletir).
- Limites de tamanho de pedido e de manifesto, nome de arquivo limpo (sem `../`), SHA-256 no fim.
- "Web of trust" (`crompressor trust <peer>`), hoje usada só no cérebro federado.
- Sem auditoria externa ainda.

O que muda e o que falta no desenho cofre ↔ cofre:

- **Autorização por pacote, não por cérebro.** Ter o mesmo cérebro não pode significar "pode ler tudo".
  Lista de máquinas confiáveis por pacote (chave pública), e convite assinado para dar acesso.
- **Vazamento por pergunta.** Perguntar "você tem o hash X?" revela que se tem o arquivo que contém X.
  Responder só a quem tem acesso ao pacote; nunca a desconhecidos.
- **Dados recebidos são suspeitos.** Conferir o SHA-256 de cada bloco antes de gravar; limitar tamanho e
  profundidade de delta (hoje já: base nunca é delta); recusar delta cuja base não existe.
- **Cifra no disco:** o cofre hoje fica sem cifra. O motor tem AES-256-GCM (`--encrypt`) para reaproveitar.
- **Versões:** `.crom` novo não abre em binário velho, e nós de versões diferentes não sincronizam. Gravar a
  versão do formato no `meta.json` e no protocolo, e negociar na conexão.

---

## 8. Peças do motor que o crom-ar pode reaproveitar

| Peça | Onde | Para quê |
|---|---|---|
| zstd 19 em C | `internal/delta/compress_max_cgo.go` | frames frios |
| AES-256-GCM | `internal/crypto` | cofre cifrado |
| Transporte libp2p, NAT, DHT, auth | `internal/network` | sync cofre ↔ cofre |
| Reed-Solomon (FEC) | `internal/network/fec.go` | partes repartidas entre pessoas (seção 9) |
| Merkle | `internal/merkle` | conferir pedaços sem baixar tudo |

E o que pode subir do crom-ar para o motor: a busca de bloco parecido por minhash (`features()` em
`internal/store/delta.go`) e o delta por bloco.

Ignorar para o cofre: modo edge (perde dados), fractal, `llm-vfs`, `cromfs` (experimentais).

---

## 9. Ideia: cofre repartido entre pessoas (só análise)

Cada pessoa guarda parte dos blocos; qualquer um com acesso remonta o arquivo pedindo as partes. Projetos
que fazem isso: IPFS, Storj, Tahoe-LAFS, Sia.

O que exige:

- **Redundância:** pessoas desligam o PC. Com Reed-Solomon, um bloco vira, por exemplo, 10 partes das quais
  quaisquer 6 remontam (o motor já tem um esboço em `fec.go`). Custa 1,7× em disco espalhado.
- **Privacidade:** quem guarda a parte não pode ler. Cifrar antes de repartir. A deduplicação entre pessoas
  diferentes e a cifra brigam: cifra por conteúdo (o mesmo bloco gera a mesma cifra) deduplica, mas revela
  "alguém tem esse arquivo".
- **Saber quem tem o quê:** índice distribuído (DHT), e conferir de tempos em tempos se as partes ainda
  existem (prova de armazenamento).
- **Incentivo e abuso:** por que alguém guardaria partes de outros? Cotas ("guarde 2× o que você usa"),
  limites, bloqueio de quem some.
- **Latência:** ler um arquivo do FUSE puxando partes de várias pessoas é lento; precisa de cache local.

Conclusão: cabe melhor num projeto separado (por exemplo dentro do `crompressor-sinapse`, que já é o
repositório de transporte P2P), em cima do sync cofre ↔ cofre. O sync entre as máquinas do próprio usuário
vem primeiro; repartir entre pessoas é a etapa seguinte.

---

## 10. Decisões abertas (do usuário)

1. `.cromar` local como **visão** do cofre (sem duplicar) — confirmar.
2. Sincronização: cofre ↔ cofre por SHA-256 e deltas (recomendado) ou cérebro fixo no `.cromar`.
3. Cérebro que só cresce como forma do "retreino automático" — confirmar.
4. Cofre repartido entre pessoas: projeto separado?
