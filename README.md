# Nagi

Nagi は、タスクの claim、隔離 worktree での実装、独立 QA、安全な cleanup、検証済み SHA の Draft PR 公開を、再開可能な operation として管理する Go CLI です。

## 再現可能な環境

```console
nix develop
go test ./...
nix flake check
go build ./cmd/nagi
```

Nix は Go、Git、GitHub CLI、SQLite CLI、`jq` を提供します。Xcode はホスト依存であり Nix へ含めません。利用可否と選択状態は JSON で確認できます。

```console
nix run . -- host-check --component xcode
nix run . -- host-check --component github
```

## 最初の run

runner 設定は信頼済み base commit 内の `.nagi.json` から読みます。値は shell 文字列ではなく argv です。

```json
{
  "argv": ["go", "test", "./..."],
  "seedFiles": []
}
```

初期化結果の `projectId` と DB は repository/worktree 外の state root に保存されます。以降の例では `PROJECT_ID` を初期化結果の値に置き換えます。

```console
nagi init --repo .
nagi db verify --project PROJECT_ID
nagi task add --project PROJECT_ID --id parent --title Parent --lane master --base master
nagi task add --project PROJECT_ID --id base-child --title Base-child --parent parent --depends parent --lane base --base master
nagi task add --project PROJECT_ID --id direct-child --title Direct-child --parent parent --lane master --base master
nagi task start --project PROJECT_ID --task parent
nagi snapshot --project PROJECT_ID
```

`task start` は desired state と operation ID を SQLite に確定してから外部副作用を開始します。停止後は次の呼び出しで同じ operation を続行します。

```console
nagi resume --project PROJECT_ID
nagi reconcile --project PROJECT_ID
```

同じ Ready task を別プロセスが先に claim 済みの場合、JSON の `reason` は `already_claimed`、終了コードは `10` です。

## 隔離・runner・seed

各 run には別々の branch、worktree、base SHA、DerivedData、runner session、ログが割り当てられます。runner には対象 worktree と run 専用環境だけを渡します。外部 seed は明示登録した snapshot だけを、base commit 内の設定に列挙された相対 path へコピーできます。

```console
nagi seed register --project PROJECT_ID --source ./local-seed.txt --name local-seed.txt
nagi run cancel --project PROJECT_ID --run RUN_ID
```

絶対 path、`..`、登録 root 外を指す path は拒否します。seed の内容、GitHub credential、token は DB、event、Nagi のログに保存しません。

## 照合と cleanup

`reconcile` は `git worktree list --porcelain -z` の実状態と SQLite を照合し、`lost_worktree`、`unmanaged`、`state_mismatch`、`dirty` を JSON 化します。これらは自動削除されません。

runner 停止、final SHA、統合または明示破棄、clean worktree、artifact 保存を再観測できた run だけ cleanup できます。通常経路は force remove、force push、branch 強制削除、未コミット変更の破棄を行いません。

```console
nagi run complete --project PROJECT_ID --run RUN_ID --disposition discarded
nagi cleanup --project PROJECT_ID --run RUN_ID
nagi events --project PROJECT_ID
```

## 独立 QA

QA packet は candidate SHA、criterion、fixture、argv、artifact、Xcode 要否だけを持つ厳格な JSON schema です。実装会話や自己評価を受け付けるフィールドはありません。

```json
{
  "candidateSha": "FULL_COMMIT_SHA",
  "xcode": "auto",
  "criteria": [
    {
      "name": "all tests",
      "fixture": "README.md",
      "argv": ["go", "test", "./..."],
      "artifacts": []
    }
  ]
}
```

```console
nagi qa run --project PROJECT_ID --run RUN_ID --packet qa-packet.json
```

QA は candidate SHA の detached worktree と専用 DerivedData で実行されます。criterion ごとの pass/fail、再現 argv、対象 SHA、artifact 参照を SQLite に保存し、全文ログ、xcresult、画像は DB 外の Artifact Store に保存します。

## Draft PR

```console
nagi pr prepare --project PROJECT_ID --run RUN_ID --target master
nagi pr sync --project PROJECT_ID --run RUN_ID
nagi pr undraft --project PROJECT_ID --run RUN_ID
```

GitHub transport は Nix が供給する `gh api` を argv で実行し、認証は利用者の `gh` 設定へ委譲します。同じ head/target の既存 PR を再利用します。独立 QA、required CI、競合なし、PR head と validated SHA の一致、必須 artifact の存在をすべて再観測できた場合だけ Draft を解除します。head が変われば以前の QA は無効です。merge、force push、レビュー指摘の自動適用は通常経路にありません。

明示した test repository で transport を smoke test する場合は、その checkout を `nagi init` し、上記の `qa run` と `pr prepare` を実行します。`pr prepare` は Draft PR までで、merge は行いません。

## テスト証拠

`go test ./...` と `nix flake check` は、決定的 fake による状態遷移、実 SQLite の複数 CLI process claim、実 Git repository の並列隔離、NUL-safe worktree 照合、dirty 非削除、cleanup 再開、detached QA、artifact 不足、PR 再試行、undraft 条件の分岐を実行します。ホスト Xcode fixture は Nix 非依存 check から分離し、`host-check` の結果が `available` の環境でだけ実行対象になります。
