# coredns-ztnet

ZTNET の API から ZeroTier ネットワークのメンバー情報を取得し、`hosts(5)` 形式の出力を生成する [zt2hosts.sh](./zt2hosts.sh) の機能を CoreDNS プラグインとして実装したものです。

承認済みメンバーの IP 割当と、ネットワークで有効化された RFC4193 / 6plane の IPv6 アドレスを、CoreDNS の A / AAAA / PTR レコードとして動的に配信します。

## 機能

- ZTNET API からネットワーク情報とメンバー一覧を定期取得(既定 30 秒)
- 承認済みメンバーのレコードを `name.zone` と `nodeid.zone` の両方で配信
- `v6AssignMode.6plane` / `rfc4193` に応じて ZeroTier の IPv6 アドレスを生成して AAAA で配信
- 逆引き (PTR) の自動生成
- 複数ネットワーク・複数ゾーンの同時サポート
- 取得失敗時は直前のデータを保持(初回失敗時は SERVFAIL)
- ゾーン apex は SOA、未知名は NXDOMAIN、型不一致は NODATA を返す権威応答

## 動作要件

- Go 1.25 以上(CoreDNS v1.14.x 準拠。`go.mod` の go ディレクティブを参照)
- ZTNET サーバー(API トークンが必要)

## ビルド

### プラグインの単体ビルド

```console
go build ./...
```

### CoreDNS への組み込み

プラグインは CoreDNS 本体にコンパイル時に組み込みます。`hack/build-coredns.sh` が
CoreDNS の指定バージョンをクローンし、プラグイン登録・モジュール置換・ビルドを一括で行います。

```console
# 引数: <CoreDNSバージョン> [出力先]
bash hack/build-coredns.sh 1.14.6 ./coredns
./coredns -plugins | grep ztnet   # ztnet が表示されれば成功
```

クロスコンパイルは環境変数で指定します(ランナー上で実行する必要がある
`go generate` は自動的にホストアーキテクチャで実行されます)。

```console
GOOS=linux GOARCH=arm64 bash hack/build-coredns.sh 1.14.6 ./coredns-arm64
```

## 設定 (Corefile)

```corefile
. {
    ztnet example.com:8056c2e21c000001 example.org:8056c2e21c000002 {
        api http://localhost:3000
        token YOUR_ZTNET_API_TOKEN
        refresh 30s
        ttl 60s
        fallthrough
    }
    log
    errors
}
```

### ディレクティブ

| 項目 | 説明 | 既定値 |
|---|---|---|
| `ztnet zone:networkID ...` | 配信するゾーンと ZeroTier ネットワーク ID の組(位置引数)。`network` ディレクティブでも指定可 | 必須 |
| `network zone networkID` | ゾーンとネットワーク ID を別々に指定 | — |
| `api` | ZTNET のベース URL(`/api/v1` が自動付与) | `http://localhost:3000` |
| `token` | API トークン(`x-ztnet-auth` ヘッダ)。未指定時は環境変数 `ZTNET_API_TOKEN` | — |
| `refresh` | データの再取得間隔(正の値のみ) | `30s` |
| `ttl` | 応答レコードの TTL | `60s` |
| `fallthrough` | ゾーン内の未知名を NXDOMAIN ではなく次のプラグインへ委譲 | 無効 |
| `insecure_skip_verify` | 自己署名証明書の TLS 検証を無効化(自己責任) | 無効 |

`token` は Corefile に直接書くか、環境変数 `ZTNET_API_TOKEN` で指定します。環境変数を
推奨します。

## CI / CD

| ワークフロー | トリガー | 内容 |
|---|---|---|
| [ci.yml](.github/workflows/ci.yml) | 全 PR / `workflow_dispatch` | `go vet` / `go test`、CoreDNS への組み込みビルドと `ztnet` プラグイン登録確認 |
| [cd.yml](.github/workflows/cd.yml) | `main` への push（PR マージ含む）/ `v*` タグ / `workflow_dispatch` | バイナリ成果物・Docker イメージの公開、タグ時は GitHub Release |

PR のマージ前チェックでは公開（push）は行いません。

## Docker イメージ

CD ワークフローがビルドし、`ghcr.io/<owner>/coredns-ztnet` に公開します。

- `main` ブランチへの push → `main` / `latest` / `coredns-<version>` / sha タグ
- `v*` タグの push → セマンティックバージョンタグ(`v1.0.0` 等) + `latest`
- プラットフォーム: `linux/amd64`, `linux/arm64`

### 実行例

Docker Runの例
```console
docker run -d --name coredns-ztnet -p 53:53/udp \
  -e ZTNET_NETWORKS="example.com:8056c2e21c000001" \
  -e ZTNET_API_TOKEN="YOUR_ZTNET_API_TOKEN" \
  -e ZTNET_API="http://192.168.1.10:3000" \
  ghcr.io/kouhei-ioroi/coredns-ztnet:latest
```

Docker Composeの例
```yaml
version: "3.8"

services:
  coredns-ztnet:
    image: ghcr.io/kouhei-ioroi/coredns-ztnet:latest
    container_name: coredns-ztnet
    ports:
      - "53:53/udp"
    environment:
      ZTNET_NETWORKS: "example.com:8056c2e21c000001"
      ZTNET_API_TOKEN: "YOUR_ZTNET_API_TOKEN"
      ZTNET_API: "http://192.168.1.10:3000"
```

### コンテナの環境変数

| 変数 | 必須 | 説明 |
|---|---|---|
| `ZTNET_NETWORKS` | 必須 | スペース区切りの `zone:networkID` の組(複数指定可) |
| `ZTNET_API_TOKEN` | 必須 | ZTNET API トークン |
| `ZTNET_API` | 任意 | ZTNET のベース URL(既定 `http://localhost:3000`) |
| `ZTNET_REFRESH` | 任意 | 再取得間隔(既定 `30s`) |
| `ZTNET_TTL` | 任意 | レコード TTL(既定 `60s`) |
| `ZTNET_FALLTHROUGH` | 任意 | 値を設定すると `fallthrough` を有効化 |

独自の Corefile を使う場合は、`ZTNET_NETWORKS` を設定せずに `/etc/coredns/Corefile`
へマウントしてください。

## GitHub Actions

- `.github/workflows/build.yml` — push / タグ / PR / 手動実行で以下を実行します
  1. 単体テスト(`go vet` + `go test -race`)
  2. バージョン解決(`go.mod` の CoreDNS バージョン。`workflow_dispatch` の `coredns_version` 入力で上書き可)
  3. バイナリビルド(`linux/amd64` と `linux/arm64`。アーティファクトとして保存)
  4. Docker イメージのビルドと GHCR への push(PR ではビルドのみ)
  5. `v*` タグ push 時に GitHub Release の作成とバイナリ添付
- `.github/workflows/check-upstream.yml` — 毎週月曜に CoreDNS の最新リリースを確認し、
  `go.mod` のバージョンより新しい場合、バージョン更新のプルリクエストを自動作成します。

### リリースの作成

バイナリと Docker イメージのリリース版を公開するには、`v*` 形式のタグを push します。

```console
git tag v1.0.0
git push origin v1.0.0
```

これにより、`ghcr.io/<owner>/coredns-ztnet:v1.0.0` と `:latest` が更新され、
GitHub Release に `linux/amd64` / `linux/arm64` のバイナリが添付されます。
**リリースジョブは両アーキテクチャのビルド成功が条件**です(片方が失敗すると
リリースは作成されません)。

## アドレス生成の互換性

RFC4193 / 6plane の IPv6 アドレスは `zt2hosts.sh` と完全に一致するよう実装しています。

- **RFC4193**: `fd` + ネットワーク ID 全 64bit + `99:93` + ノード ID 上位 40bit
- **6plane**: `fc` + ネットワーク ID 上位 32bit と下位部の XOR + ノード ID 上位 40bit + `0000:0000:0001`

## 開発

```console
go test ./...        # 単体テスト
go test -race ./...  # レース検出付き
go vet ./...
```

## 関連

- [zt2hosts.sh](./zt2hosts.sh) — 本プラグインの元となったシェルスクリプト
- [ZTNET](https://ztnet.network/) — ZeroTier 用の Web 管理 UI
