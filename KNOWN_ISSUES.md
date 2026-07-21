# Known Issues — 既知の安全性問題

> 最終検証: 2026-07-21 / 対象 commit `9383cfe`
>
> This file is the **live, authoritative status** of the safety issues found in the
> 2026-07-07 safety review. The frozen report with full evidence and reproduction
> scenarios is [docs/safety-review-2026-07-07.md](docs/safety-review-2026-07-07.md).
> Update this file (not the report) whenever an issue's status changes.

このファイルは [docs/safety-review-2026-07-07.md](docs/safety-review-2026-07-07.md)
（2026-07-07 時点の安全性レビュー報告書・凍結）で確認された問題の**現在のステータス表**です。
修正が main にマージされたら、該当行の状態と修正 commit をこのファイルで更新してください。
報告書そのものは編集しないこと。

## サマリ

| 状態 | 件数 |
|---|---|
| ✅ FIXED | 5（B1, B2, C1, C2, C4） |
| 🟠 PARTIAL | 1（C3） |
| ❌ UNFIXED | 16 |

**実用上の含意**: ログ圧縮（グループ A）は通常運転で安全性が破れるため、
`MaxRaftState=0`（圧縮無効）以外で運用してはならない。リース読みは
リーダー交代前後で stale read を返しうる（D1–D3）。クライアントのリトライは
二重適用になりうる（D4/D5）。

## グループ A: ログ圧縮のインデックス体系（すべて通常運転で発火しうる）

| ID | 概要 | 状態 | 根拠（現コード） |
|---|---|---|---|
| A1 | AppendEntries 受信側が LastIncludedIndex オフセット未対応 → 圧縮済みフォロワーのログ恒久破壊 | ❌ UNFIXED | `raft/rpc.go:146-172` |
| A2 | 投票経路が snapshot を無視し「空ログ」を名乗る → Leader Completeness 違反 | ❌ UNFIXED | `raft/rpc.go:81-88, 214-218` |
| A3 | 圧縮済みノード当選時に NextIndex が相対/絶対混同 → クラスタ書き込み不能 | ❌ UNFIXED | `raft/state.go:274`, `raft/rpc.go:367-376, 482-484` |
| A4 | updateCommitIndex が相対長と絶対 CommitIndex を比較 → リーダー圧縮後コミット恒久停止 | ❌ UNFIXED | `raft/rpc.go:529-531` |
| A5 | 再起動時に LastApplied がスナップショットから復元されない → 二重適用・範囲外 panic | ❌ UNFIXED | `raft/state.go:143, 169-181`, `raft/log.go:207-209` |
| A6 | production で Snapshotter 未配線（3 層のギャップ）→ InstallSnapshot が発火しない/必ず失敗 | ❌ UNFIXED | `main.go:240-258`, `persistence/kv_snapshotter.go:36-83`, `kvstore/store.go:305-309` |
| A7 | InstallSnapshot 受信側が分岐 suffix を term 検査なしで保持 → Log Matching 違反 | ❌ UNFIXED | `raft/rpc.go:623-630` |
| A8 | フォロワー側スナップショットが永続化されない → クラッシュで恒久復元不能 | ❌ UNFIXED | `kvstore/store.go:303-319` |

## グループ B: コミット済みエントリの喪失

| ID | 概要 | 状態 | 根拠（現コード）/ 修正 commit |
|---|---|---|---|
| B1 | applyCh 満杯時にコミット済みコマンドを黙って破棄 | ✅ FIXED | `9a90cf5`（ブロッキング送信化。ただしトレードオフあり — 下記「注記 1」） |
| B2 | AppendEntries の無条件切り詰めでコミット済み suffix が消える（論文 §5.3 step 3 違反） | ✅ FIXED | `7151e77`（論文 §5.3 step 3 準拠の conflict ベース切り詰め） |
| B3 | InstallSnapshot が rs.mu 保持のまま applyCh へブロッキング送信（PLAUSIBLE） | ❌ UNFIXED | `raft/rpc.go:590-591, 653-662` |

## グループ C: 永続化規律

| ID | 概要 | 状態 | 根拠（現コード）/ 修正 commit |
|---|---|---|---|
| C1 | RequestVote が term/votedFor を persist しない → 同一 term に 2 リーダー | ✅ FIXED | `2a35ce9`（PR #11、応答前 persist + 失敗時 VoteGranted=false） |
| C2 | ハートビート経由の term 更新が persist されない | ✅ FIXED | `2a35ce9`（降格パス 3 箇所も対応） |
| C3 | persist() のエラー無視 | 🟠 PARTIAL | RPC 応答経路は修正済み（`2a35ce9`）。**リーダー自身の `AppendLogEntry`（Start() 経由の本番経路）と `TruncateLogAfter` はエラーを無視して続行**: `raft/log.go:136-139, 190-192` |
| C4 | 永続状態ロード失敗で「記憶喪失ノード」として参加 | ✅ FIXED | `2a35ce9`（ロード失敗時は起動拒否 `main.go:252-255`） |

## グループ D: 読み取り・クライアント処理の linearizability

| ID | 概要 | 状態 | 根拠（現コード） |
|---|---|---|---|
| D1 | リース期間にランダム electionTimeout を流用 → stale read | ❌ UNFIXED | `raft/state.go:317-333` |
| D2 | リース起点が応答受信時刻（送信時刻でなく）→ 違反窓が拡大 | ❌ UNFIXED | `raft/rpc.go:429-434` |
| D3 | 当選時 no-op エントリなし（論文 §8 違反、最も再現容易な stale read） | ❌ UNFIXED | `raft/rpc.go:293-306`, `raft/state.go:268-281` |
| D4 | 重複検出（ClientID/SeqNum）が実 API 経路から未配線 → リトライで二重適用 | ❌ UNFIXED | `kvstore/store.go:457-463` |
| D5 | 適用成功後に spurious な "leadership lost" エラー → 不要リトライを誘発 | ❌ UNFIXED | `kvstore/store.go:483-489` |

## グループ E: data race

| ID | 概要 | 状態 | 根拠（現コード） |
|---|---|---|---|
| E1 | startElection がロック外で ResetElectionTimer を呼ぶ | ❌ UNFIXED | `raft/rpc.go:223-225`（`2a35ce9` が persist 失敗パスに同種の呼び出しをもう 1 箇所追加: `raft/rpc.go:206-210`） |
| E2 | 送信エントリの backing array を ロック外 marshal 中にハンドラが書き換えうる | ❌ UNFIXED | `raft/rpc.go:378-383` vs `165-173` |

## 注記（レビュー後に判明した事実）

1. **B1 修正のトレードオフ**: ブロッキング送信化により、applyCh 逆圧時（例: applyLoop 内の
   同期スナップショット保存中）は rs.mu 保持のまま全 RPC・選挙処理が停止する liveness 問題に
   転化した。専用 applier goroutine への分離が対策（TODO.md の項目 2.5）。
2. **`2a35ce9` の副作用**: AppendEntries でメモリ上のログを切り詰め+追記した後に persist が
   失敗すると `Success=false` を返すが、メモリとディスクの不一致が次回 persist 成功まで残る
   （安全側の挙動）。
3. **RequestVote の persist 失敗時**: メモリ上の VotedFor は保持したまま `VoteGranted=false`
   のみ返す。同一 term 内の再投票を防ぐ安全方向の意図的設計（`raft/rpc.go:96-105` のコメント参照）。
4. **A6 が CI で検出されない理由**: 統合テストは `fakeSnapshotter` を使い、
   (i) SetSnapshotter 未配線、(ii) インターフェース非互換、(iii) V2 形式 unmarshal 不整合の
   3 つのギャップをすべて迂回している。

## 修正の推奨順序

報告書の推奨（B1 → C 群 → B2 → A 群 → D3/D1/D2 → D4/D5）のうち B1 と C 群の大半は完了。
残りは **C3 完遂 → B2 → A 群 → D3 → D1/D2 → D4/D5 → E 群** の順を推奨。
当面の安全な暫定策は `MaxRaftState=0`（圧縮無効）での運用。
