# Raft論文実装状況比較

> 最終検証: 2026-09-06 against commit `980f43d`

本ドキュメントは [Raft論文](https://raft.github.io/raft.pdf) の内容と rosetta プロジェクトの実装状況を比較したものです。本プロジェクトは学習目的の実装であり、既知の安全性違反は `KNOWN_ISSUES.md`（`docs/safety-review-2026-07-07.md` および `docs/raft-audit-2026-09-06.md` の再監査結果を反映した現在のステータス表）に集約されています。

## 概要サマリー

| カテゴリ | 状態 | 備考 |
|---------|------|------|
| リーダー選挙 (Section 5.2) | ✅ 実装済み | 基本動作は実装済み。当選時に current-term no-op を追記（D3 解消）。圧縮後の投票判定も絶対 index 化済み（A2 解消・commit `8ad5367`）。降格処理は `becomeFollowerLocked` に一本化され、高 term を見て降格した元 leader も選挙タイマーが再始動する（R6 解消・`ac93fcb`）。選挙タイマーの更新も `rs.mu` 下に統一（E1 解消・`c5fdc0f`） |
| ログ複製 (Section 5.3) | ⚠️ 要修正あり | step 3 の conflict ベース切り詰め（B2 解消・commit `7151e77`）、受信側の圧縮後 index 対応（A1 解消・commit `8ad5367`）は完了。ただし境界 term 検査・commit 上限の一部分岐が未検証（R13、`raft/rpc.go:153-163,191-194`） |
| 安全性保証 (Section 5.4) | ⚠️ 要修正あり | 選挙制限は実装済み。B2・A2 は解消済み（`7151e77` / `8ad5367`）。A7（InstallSnapshot 受信側の §7 保持ルール）も解消（`019d33e`）。Log Matching（R1: `Start` の leader 確認と append の原子化・`2c26b9a`）と Leader Completeness（R2: 未永続エントリの duplicate ACK・`c362ae4`）も解消。State Machine Safety の R3–R5（snapshot の世代整合・古い snapshot の適用）も解消（`0695b95` / `f53617e` / `156510a`）。E2（送信エントリの backing array 共有）も送信前コピーで解消（`7e3eb61`）。残る懸念は R13（境界 term 検査・commit 上限の未検証分岐） |
| 永続化 (Figure 2) | ✅ 実装済み | RPC 応答前の persist 規律あり（C1/C2/C4 解消・commit `2a35ce9`）。リーダー自身の追記経路（`appendEntryLocked` 経由の `AppendLogEntry`/`Start`/当選時 no-op、`TruncateLogAfter`）も persist 失敗をロールバックしてエラー通知（C3 解消・commit `ffc2926`）。AppendEntries 受信経路の未永続 merge も persist 失敗時にロールバックするようになった（R2 解消・commit `c362ae4`）。InstallSnapshot 受信経路と `TruncateLogTo` も同じロールバック規律に揃えた（R3 解消・commit `0695b95`）。加えて raft_state.json と snapshot.json の世代整合を起動時に検証し、復旧不能な組み合わせでは起動を拒否する |
| ログコンパクション (Section 7) | ⚠️ 要修正あり（chunk 転送のみ） | 絶対 index 統一・投票/コミット/適用経路・本番配線・フォロワー側永続化（A1–A6, A8・`8ad5367`/`d0cbdc1`/`c516f54`）、受信側の §7 保持ルール（A7・`019d33e`）に加え、2026-09-06 再監査の 3 件も解消: 受信経路の世代整合と起動時検証（R3・`0695b95`）、メタデータとペイロードの同一世代化（R4・`f53617e`）、古い snapshot の適用禁止（R5・`156510a`）。安全性の未修正項目はない。B3（`rs.mu` 保持下での `applyCh` 送信、liveness）も専用 applier goroutine への分離で解消した（`f873d9b`）。残るのは R15（chunk 転送なし） |
| クラスタメンバーシップ変更 (Section 6) | ❌ 未実装 | Joint consensus未対応（R14）。`-join` は失敗してもログのみで起動を続ける fail-open（R12） |
| クライアント相互作用 (Section 8) | ⚠️ 条件付きで配線済み | 重複検知（ClientID/SeqNum）を実 API 経路へ配線（D4 解消・commit `52afd48`）。ただし dedup は `ClientID` を指定した場合のみ発動する条件付きで（`kvstore/store.go:386`）、無条件の at-most-once ではない。committed 済みの結果解決は log index ではなく opID ベースで、pending 登録のタイミング競合（R9）がある。batch API は実装されておらず空 PUT として黙って成功する（R11） |
| 読み取り専用クエリ最適化 | ✅ 線形化実装 | ReadIndex プロトコル + 当選時 no-op で linearizable read を実装。旧リース方式は撤去（D1〜D3 解消） |

（A1〜E2 の ID は see ../KNOWN_ISSUES.md を参照。R1〜R18 は 2026-09-06 再監査 `docs/raft-audit-2026-09-06.md` で新規に確認された ID、R19 以降はその修正作業中に見つかった ID で、詳細は KNOWN_ISSUES.md のグループ R を参照）

> **現在の未修正**: グループ R のうち R9–R16, R18（計 9 件）。B3（`applyCh` への送信を `rs.mu` 保持のまま行う liveness 問題）と R19（`Kill` が goroutine の終了を待たない）は解消した（`f873d9b` / `7c96f14` / `13570d5`）。監査が P0 とした R1–R5 はすべて解消し（`2c26b9a` / `c362ae4` / `0695b95` / `f53617e` / `156510a`）、P1 の R6 と data race 2 件（E1/E2）も解消した（`ac93fcb` / `c5fdc0f` / `7e3eb61`）。ただし R13（AppendEntries の境界 term 検査・commit 上限）のように安全性に触れうる未修正項目が残るため、「論文の安全性性質を破る既知の経路は残っていない」とはまだ言えない。本プロジェクトは教育用途であり、本番運用可ではない。

---

## 詳細比較

### 1. リーダー選挙 (Section 5.2) ✅ 実装済み

#### 論文の要件
- サーバーはFollower → Candidate → Leaderの状態遷移を行う
- 選挙タイムアウトはランダム化してスプリットボートを防ぐ
- Candidateは自身に投票し、他ノードにRequestVote RPCを送信
- 過半数の票を得たらリーダーになる
- 任期(term)は論理クロックとして機能

#### 実装状況
**ファイル**: `raft/state.go`, `raft/rpc.go`, `raft/node.go`

```go
// raft/state.go:13-17 - 状態定義
const (
    Follower NodeState = iota
    Candidate
    Leader
)

// raft/state.go:136 - ランダム化された選挙タイムアウト (150-300ms)
randomTimeout := electionTimeoutBaseMs + rand.Intn(electionTimeoutJitterMs)

// raft/state.go:255-262 - リセット時も再ランダム化
func (rs *RaftState) ResetElectionTimer() {
    randomTimeout := electionTimeoutBaseMs + rand.Intn(electionTimeoutJitterMs)
    rs.electionTimeout = time.Duration(randomTimeout) * time.Millisecond
    ...
}
```

**RequestVote RPC構造体** (`raft/rpc.go:11-16`):
```go
type RequestVoteArgs struct {
    Term         int    // 候補者の任期
    CandidateID  string // 候補者ID
    LastLogIndex int    // 最後のログエントリのインデックス
    LastLogTerm  int    // 最後のログエントリの任期
}
```

状態遷移・ランダム化タイムアウト・過半数当選・投票と任期の応答前 persist（`raft/rpc.go:101-106`、`startElection` の `raft/rpc.go:200-212`）は実装済みです。個別項目の現状は以下の通りです:

- **当選時の no-op エントリを追記**（✅ 解消）: 当選処理は `becomeLeader`（`raft/noop.go`）に集約され、リーダー遷移時に current-term の no-op エントリ（`NoOpCommand`、`appendNoOpLocked`）を追記します。`startElection` / `requestVoteFromPeer` の当選経路も旧来の手書き遷移から `becomeLeader` 呼び出しに置換されました。これにより論文 Section 8 の就任時 no-op コミットを満たし、(a) 前任 term のコミット済みエントリを Log Matching 経由で安全に advance でき、(b) §6.4 ReadIndex の前提（current term で 1 件コミット済み）を選挙直後に満たします。修正 commit `60fd631`。see ../KNOWN_ISSUES.md (D3)
- **投票判定がスナップショットを考慮**（✅ 解消）: `RequestVote` / `startElection` は絶対 index（`LastIncludedIndex/Term` を含む last log index/term）で候補者ログの新しさを §5.4.1 に沿って評価するようになりました。ログ圧縮後も「空ログ」を名乗らず、Leader Completeness を保ちます。修正 commit `8ad5367`。see ../KNOWN_ISSUES.md (A2)
- **降格処理の一本化**（✅ 解消）: follower へ落ちる経路はすべて `becomeFollowerLocked`（`raft/state.go`）を通ります。state=Follower・term 更新と VotedFor=nil・`currentLeader`・`rs.leader = nil`・選挙タイマー再始動・persist を 1 箇所にまとめ、降格 3 経路（`requestVoteFromPeer`/`replicateToPeer`/`sendSnapshotToPeer`）と `stepDown`、受信 3 経路（RequestVote/AppendEntries/InstallSnapshot）、`startElection` の persist 失敗経路が共有します。`becomeLeader` が停止した選挙タイマーが降格時に再始動するようになり、高 term を見て降格した元 leader も自力で立候補できます。persist は term が動いたときだけ行うので heartbeat 経路にディスク I/O は増えません。修正 commit `ac93fcb`。see ../KNOWN_ISSUES.md (R6)
- **選挙タイマーの mutex 統一**（✅ 解消）: `resetElectionTimerLocked`（`rs.mu` 保持が前提）と、自分でロックを取る公開 `ResetElectionTimer` に分けました。`startElection` は term を刻むのと同じ臨界区間でタイマーを再始動します。修正 commit `c5fdc0f`。see ../KNOWN_ISSUES.md (E1)

---

### 2. ログ複製 (Section 5.3) ✅ 実装済み

#### 論文の要件
- リーダーはクライアントコマンドをログに追加
- AppendEntries RPCでフォロワーにログを複製
- 過半数に複製されたらコミット
- nextIndex と matchIndex でフォロワーの状態を追跡
- ログ整合性チェック (prevLogIndex, prevLogTerm)

#### 実装状況
**ファイル**: `raft/rpc.go`, `raft/log.go`

```go
// raft/rpc.go:23-30 - AppendEntries RPC構造体
type AppendEntriesArgs struct {
    Term         int        // リーダーの任期
    LeaderID     string     // リーダーID
    PrevLogIndex int        // 直前のログエントリのインデックス
    PrevLogTerm  int        // 直前のログエントリの任期
    Entries      []LogEntry // 複製するエントリ
    LeaderCommit int        // リーダーのコミットインデックス
}

// raft/state.go - リーダー状態
type LeaderState struct {
    NextIndex  map[string]int  // 各フォロワーへの次送信インデックス
    MatchIndex map[string]int  // 各フォロワーの複製済みインデックス
    inFlight   map[string]bool // 送信中の peer（tick はここが true の peer を skip）
}
```

複製 RPC は peer ごとに直列化されています（`499c4b8`）。以前は 50ms の tick ごとに peer 単位の
goroutine を無条件に起動していたため、同一 follower に AppendEntries の列と 5 秒 timeout の
InstallSnapshot が並走し、応答が逆順に戻ると `MatchIndex = prevLogIndex + len(entries)` の
無条件代入でその follower の進捗が巻き戻っていました。現在は `sendHeartbeats` が送信中フラグを
term/commit index を読むのと同じ `rs.mu` 臨界区間で立てて busy な peer を skip し、成功応答での
`MatchIndex`/`NextIndex` は後退しません（`MatchIndex` は high-water mark。fast rollback の
失敗経路は従来どおり `NextIndex` を下げます）。

#### Fast Rollback最適化 ✅ 実装済み

論文のSection 5.3で言及されている最適化（フォロワーが競合情報を返してリーダーがスキップする）も実装済み:

```go
// raft/rpc.go:32-39
type AppendEntriesReply struct {
    Term    int
    Success bool
    // Fast rollback optimization (Section 5.3)
    ConflictTerm  int  // 競合エントリの任期
    ConflictIndex int  // ConflictTermの最初のインデックス
}
```

リーダー側の処理 (`handleReplicationConflict`, `raft/rpc.go:443-471`):
- `ConflictTerm == -1`: フォロワーのログが短い → `ConflictIndex`にジャンプ
- それ以外: リーダーのログで`ConflictTerm`を検索して最適な位置にスキップ

基本の複製フローと fast rollback は実装済みです。かつての論文違反は解消されました:

- **AppendEntries step 3 の conflict ベース切り詰め**（✅ 解消）: 受信ハンドラは「同一 index で term が異なる最初のエントリ以降のみ削除」する §5.3 step 3 準拠の切り詰めに修正され、既存エントリと一致する suffix は保持します。HTTPトランスポートで遅延・重複配送された古い AppendEntries がコミット済み suffix を削除する経路が塞がれました。修正 commit `7151e77`。see ../KNOWN_ISSUES.md (B2)
- **受信ハンドラが圧縮後のインデックス体系に対応**（✅ 解消）: 受信ハンドラは `PrevLogIndex` を `LastIncludedIndex` オフセットに合わせて絶対 index として扱い、圧縮境界を越えない一貫性チェック・衝突探索を行うようになりました。圧縮済みフォロワーへの複製がログを破壊する経路は解消されています。修正 commit `8ad5367`。see ../KNOWN_ISSUES.md (A1)

---

### 3. 安全性保証 (Section 5.4) ⚠️ 要修正あり（R13）

#### 論文の要件
- **選挙安全性**: 各任期で最大1人のリーダー
- **リーダー追記のみ**: リーダーはログを上書き・削除しない
- **ログ一致**: 同じインデックス・任期のエントリがあれば、それ以前も一致
- **リーダー完全性**: 過去にコミットされたエントリは選出されるリーダーに含まれる
- **状態機械安全性**: 適用されたエントリは全サーバーで同一結果

#### 実装状況

**選挙制限** (`raft/rpc.go:87-93`):
```go
// 候補者のログが自分と同等以上に新しい場合のみ投票する
if args.LastLogTerm > lastLogTerm ||
    (args.LastLogTerm == lastLogTerm && args.LastLogIndex >= lastLogIndex) {
    rs.persistent.VotedFor = &args.CandidateID
    reply.VoteGranted = true
    ...
}
```

選挙制限そのものと、現 term のエントリのみをコミットカウントする §5.4.2 規則（`updateCommitIndex`, `raft/rpc.go:567-575`）は実装済みです。ただし各安全性特性の実際の成立状況は以下の通りです:

- **選挙安全性**: 投票の応答前 persist（commit `2a35ce9`）により、クラッシュ跨ぎの二重投票は防止されます
- **リーダー追記のみ**: `RaftState.Start`（`raft/log.go:148-165`）が leader 判定・term stamp・persist を 1 回の `rs.mu` の中で行うため、降格後に command が追記されることはありません（R1 解消・`2c26b9a`）
- **ログ一致 (Log Matching)**: AppendEntries の切り詰めは §5.3 step 3 準拠に修正済み（B2 解消・`7151e77`）。InstallSnapshot 受信側の分岐 suffix 保持も §7 の保持ルール実装で解消（A7・`019d33e`）。2026-09-06 再監査で指摘された R1（`RaftNode.Start` が leader/term の確認と durable append を別の `rs.mu` 臨界区間で行い、間に降格が挟まると非 leader が新 term の command を追記しうる）は `2c26b9a` で解消した。`RaftNode.Start` は `RaftState.Start` へ委譲するだけになり、role 確認から persist・失敗時ロールバックまでが単一の `rs.mu` 臨界区間に収まっている
- **リーダー完全性 (Leader Completeness)**: 選挙制限が絶対 index でスナップショットメタデータを考慮するようになり（A2 解消・commit `8ad5367`）、圧縮後もコミット済みエントリを持たないノードは当選しません。加えて当選時 no-op の追記（`becomeLeader`、D3 解消、commit `60fd631`）により、前任 term のコミット済みエントリは選挙直後に advance されます。2026-09-06 再監査の R2（persist に失敗した AppendEntries の再送が `mergeLogEntries` の「既に一致」判定で persist をスキップし `Success=true` を返し、未永続のエントリが `MatchIndex` に反映されうる）は `c362ae4` で解消した。persist 失敗時に merge 前のログへロールバックするため、再送も改めて merge → persist を通り、成功するまで `Success=false` が返る
- **状態機械安全性**: R1・R2 に続き、snapshot 経路の R3–R5 も解消しました。受信経路は「KV payload を durable 化 → Raft 境界を durable 化 → メモリ適用」の順序不変条件を守り（R3・`0695b95`、`RaftState.InstallSnapshot` の doc comment に明記）、crash は必ず復旧可能な側（snapshot が Raft より新しい）に倒れます。復旧不能な組み合わせは起動時に `persistence.VerifySnapshotConsistency` が拒否します。送信側は index/term/data を 1 つの envelope として扱い（R4・`f53617e`）、受信側は Raft も KV も適用済みインデックス以下の snapshot を無視します（R5・`156510a`）。E2（送信 slice の data race）も送信前コピーで解消しました（`7e3eb61`）。ただし R13（AppendEntries の境界 term 検査と commit 上限の未検証分岐）は未修正のため、無条件に成立するとは言えません

---

### 4. 永続化 (Figure 2) ✅ 実装済み（ロールバック規律・世代検証まで）

#### 論文の要件
- `currentTerm`, `votedFor`, `log[]` は永続化必須
- **RPC に応答する前に**安定ストレージへ書き込む
- サーバー再起動時に復元

#### 実装状況
**ファイル**: `raft/state.go`

```go
// raft/state.go:52-60
type PersistentState struct {
    CurrentTerm int
    VotedFor    *string
    Log         []LogEntry
    // スナップショットメタデータ
    LastIncludedIndex int
    LastIncludedTerm  int
}

// raft/state.go:73-76
type Persister interface {
    SaveRaftState(state *PersistentState) error
    LoadRaftState() (*PersistentState, error)
}
```

**応答前 persist 規律**（commit `2a35ce9` で確立）:
- `RequestVote`: term/投票の変更を persist してから応答。**persist 失敗時にメモリ上の VotedFor を「取り消す」ことはしない** — VotedFor はセットされたまま `VoteGranted=false` のみ返す。これは同一 term 内の再投票を防ぐ意図的な安全側の設計（`raft/rpc.go:94-104`。KNOWN_ISSUES.md 注記 3 も参照）
- `AppendEntries`: term 変更を persist してから応答する。失敗時は `Success=false`。追記エントリの persist も応答前に行われ、失敗時は merge 前のログへロールバックしたうえで `Success=false` のまま return する（`raft/rpc.go:178-207`。R2 解消・`c362ae4`）。ロールバックにより「ハンドラを抜けた時点でメモリとディスクが一致する」不変条件が保たれるため、再送された同一要求も改めて merge → persist を通り、永続化に成功するまで `Success=true` を返さない。`mergeLogEntries`（`raft/rpc.go:230-257`）は衝突位置で cap を切って（`Log[:pos:pos]`）追記するので既存エントリを上書きせず、保存しておいた slice header がそのまま復元先になる
- `startElection`: 立候補（term+1・自己投票）を persist できなければ選挙を中止（`raft/rpc.go:259-277`）
- 永続状態のロード失敗時は起動を拒否（`raft/state.go:157-161`, `raft/node.go:29-34`）

**リーダー自身の追記経路**（commit `ffc2926` で C3 を解消）:
- 追記本体は `appendEntryLocked`（`rs.mu` 保持が前提）に切り出されている。persist に失敗したら追記をロールバックし（メモリとディスクを一致させる）、エラーを返す（`raft/log.go:100-118`）。`AppendLogEntry`（`:120-129`）はこれをロック取得付きで包んだもの
- `TruncateLogAfter` は `error` を返す。persist に失敗したら切り詰め前のログを復元する（`raft/log.go:215-232`）
- 当選時 no-op（`appendNoOpLocked`）も同じ `appendEntryLocked` を使い、persist 失敗時はロールバック。no-op を失っても安全側で、current-term のエントリがコミットされるまで読み取りが `ErrNoCurrentTermCommit` を返し続けるだけ（`raft/noop.go:48-54`）
- `RaftNode.Start` はエラーを返し、KV 層はタイムアウトを待たずに操作を失敗させる（`raft/node.go:94-119`）。leader 判定と追記は `RaftState.Start` の単一 `rs.mu` 臨界区間で行われる（R1 解消・`2c26b9a`）

**snapshot 経路のロールバックと世代検証**（commit `0695b95` で R3 を解消）:
- `InstallSnapshot` 受信側は persist に失敗したら Log／`LastIncludedIndex`／`LastIncludedTerm`／`CommitIndex`／`LastApplied` をすべて呼び出し前の値へ戻す。`TruncateLogTo` も同様（`raft/snapshot.go`）。これで AppendEntries と同じ「ハンドラを抜けた時点でメモリとディスクが一致する」不変条件が snapshot 経路にも適用される
- `raft_state.json` と `snapshot.json` の間にファイル跨ぎの atomicity はないため、代わりに書き込み順序で保証する: KV payload → Raft 境界 → メモリ適用。crash は必ず「snapshot が Raft より新しい」復旧可能な側に倒れる
- 起動時に `persistence.VerifySnapshotConsistency`（`persistence/consistency.go`）が 2 ファイルの境界を比較し、Raft 境界が snapshot より進んでいれば起動を拒否する（C4 と同じ fail-closed）。逆向きは applyLoop の単調性ガードが吸収する

---

### 5. ログコンパクション / スナップショット (Section 7) ⚠️ 要修正あり（R15 の chunk 転送）

#### 論文の要件
- スナップショットで状態機械の状態をキャプチャ
- スナップショット以前のログエントリを破棄
- InstallSnapshot RPCで遅れたフォロワーにスナップショットを送信

#### 実装状況
**ファイル**: `raft/snapshot.go`, `raft/rpc.go`

```go
// raft/rpc.go:41-47 - InstallSnapshot RPC
type InstallSnapshotArgs struct {
    Term              int    // リーダーの任期
    LeaderID          string
    LastIncludedIndex int    // スナップショットに含まれる最後のインデックス
    LastIncludedTerm  int    // そのエントリの任期
    Data              []byte // スナップショットデータ
}

// raft/snapshot.go - スナップショット関連メソッド
func (rs *RaftState) TakeSnapshot(...)
func (rs *RaftState) InstallSnapshotFromData(...)
func (rs *RaftState) ShouldTakeSnapshot(...) bool
```

**ApplyMsgのスナップショットサポート** (`raft/state.go:114-124`):
```go
type ApplyMsg struct {
    CommandValid bool
    Command      interface{}
    CommandIndex int
    // スナップショット用
    SnapshotValid bool
    SnapshotIndex int
    SnapshotTerm  int
    SnapshotData  []byte
    // Raft 層が payload を先に永続化済みかどうか（R3）
    SnapshotPersisted bool
}
```

RPC 構造体・スナップショット取得（`TakeSnapshot`/`TruncateLogTo`）・リーダー送信側のインデックス変換に加え、受信・投票・コミット・適用の各経路、本番配線、フォロワー側永続化、受信側の §7 保持ルールがすべて解消されました。**グループ A に未修正の安全性課題はありません**。2026-09-06 の再監査がこの経路に見つけた R3–R5 も解消済みです:

- **受信ハンドラの圧縮後 index 対応 (A1・✅ 解消)**: `AppendEntries` ハンドラは `PrevLogIndex` を `LastIncludedIndex` オフセットに合わせて絶対 index として扱うようになりました。修正 commit `8ad5367`
- **投票経路の絶対 index 化 (A2・✅ 解消)**: `RequestVote`/`startElection` が `LastIncludedIndex/Term` を含む絶対 index で候補者ログの新しさを評価します。修正 commit `8ad5367`
- **リーダー当選時 NextIndex の絶対 seed (A3・✅ 解消)**: `initializeLeaderState` が NextIndex を絶対 index で seed します。修正 commit `8ad5367`
- **コミット判定の絶対 index 化 (A4・✅ 解消)**: `updateCommitIndex` が絶対 last index でコミットを前進させ、リーダー圧縮後もコミットが停止しません。修正 commit `8ad5367`
- **再起動時の LastApplied 復元 (A5・✅ 解消)**: `loadPersistentState` が CommitIndex/LastApplied をスナップショットから復元し、範囲外ガードも入りました。二重適用・範囲外 panic は解消されています。修正 commit `8ad5367`
- **本番構成で InstallSnapshot が配線済み (A6・✅ 解消)**: `RaftSnapshotter` を `SetSnapshotter` で本番配線し、型アサーションでインターフェース互換を担保、V2 形式もパースします。修正 commit `d0cbdc1` + `c516f54`
- **受信側の §7 保持ルール (A7・✅ 解消)**: `InstallSnapshot` ハンドラは `logAfterSnapshot`（`raft/log.go`）を通し、自ログの `LastIncludedIndex` が index/term ともにスナップショットと一致するときだけ以降を保持し、不一致（および照合対象を持たないほど遅れている場合）はログを全破棄します。論文 §7 Figure 13 の受信ルール 6/7 準拠。修正 commit `019d33e`
- **フォロワー側スナップショットの永続化 (A8・✅ 解消)**: `installSnapshotFromApplyMsg` が `saveSnapshot` でフォロワー側スナップショットをディスクに永続化します。修正 commit `c516f54`

see ../KNOWN_ISSUES.md (A1〜A8)。2026-09-06 再監査分の解消状況:

- **R3（✅ 解消・`0695b95`）Raft state と KV snapshot の世代整合**: 受信ハンドラは「①KV payload を `Snapshotter.InstallSnapshot` で durable 化 → ②Raft 境界を persist → ③`applyCh` でメモリ適用」の順序不変条件を守ります（`RaftState.InstallSnapshot` の doc comment に明記）。2 ファイル間の atomicity は依然ありませんが、この順序により crash は必ず「snapshot が Raft より新しい」復旧可能な側に倒れます。①②いずれの失敗でもメモリはロールバックされ、`ApplyMsg.SnapshotPersisted` で KV 側の二重書きを抑止します。起動時は `persistence.VerifySnapshotConsistency` が復旧不能な組み合わせ（Raft 境界 > snapshot）で起動を拒否します
- **R4（✅ 解消・`f53617e`）snapshot メタデータとペイロードの同一世代化**: `Snapshotter.ReadSnapshot` が `*raft.SnapshotData`（index/term/data）の immutable envelope を返し、`sendSnapshotToPeer` は `InstallSnapshotArgs` と follower の `MatchIndex`/`NextIndex` をその envelope だけから組み立てます。ロック内で読んだ境界は「snapshot を送るべきか」の判定にのみ使います
- **R5（✅ 解消・`156510a`）古い snapshot による状態の後退**: Raft 側は `args.LastIncludedIndex <= volatile.LastApplied` の snapshot を無視し（term 更新と election timer reset は有効な leader 通信として行う）、KV 側 `installSnapshotFromApplyMsg` も `msg.SnapshotIndex <= lastAppliedIndex` を無視する単調性ガードを持ちます
- **B3（✅ 解消・`f873d9b`）受信ハンドラのブロッキング送信**: ③のメモリ適用は専用の applier goroutine に引き渡すだけになり、`rs.mu` を保持したまま state machine を待つことはなくなりました。①②は従来どおりハンドラ内で、この順序のまま完了します。R15（chunk 転送なし）は未実装のまま残ります

なお `MaxRaftState=0`（圧縮無効）は設定として指定できません（`Config.Validate` が正数を強制）。詳細は KNOWN_ISSUES.md グループ R を参照。

---

### 6. クラスタメンバーシップ変更 (Section 6) ❌ 未実装

#### 論文の要件
- **Joint Consensus**: 新旧設定の両方で過半数を必要とする2段階プロセス
- Cold,new → Cnew の遷移
- 設定変更はログエントリとして複製

#### 現状
`network/discovery.go` に `ClusterManager` が存在しますが、これはHTTPレベルでのノード参加/離脱のみを処理:

```go
// network/discovery.go - HTTP経由のクラスタ管理
func (cm *ClusterManager) JoinCluster(existingNodeAddr string) error
func (cm *ClusterManager) LeaveCluster()
```

**問題点**:
- Raftレイヤーでの設定変更ログエントリなし
- Joint Consensus未実装
- メンバーシップ変更中の安全性保証なし

さらに、`-join` フラグ（`main.go:285-289`）は失敗してもログ出力のみで起動を継続する fail-open であり、`ClusterManager` に登録されたノードは Raft の quorum には反映されません（R12）。`StartDiscovery`（`network/discovery.go:99`）は通常起動経路から呼ばれず、実用的な参加経路は存在しません。

❌ **実装が必要（R14: joint consensus、R12: join の fail-open）**

---

### 7. クライアント相互作用 (Section 8) ⚠️ 条件付きで配線済み

#### 論文の要件
- クライアントはリーダーにコマンドを送信
- リーダーは過半数に複製後、結果を返す
- **重複検知**: クライアントIDとシーケンス番号で再実行を防止
- **Linearizability**: 各操作は呼び出しと応答の間に一度だけ適用

#### 実装状況

**リーダーのみ書き込み** ✅:
- 非リーダーノードは503レスポンスでリダイレクト

**コマンド構造体** (`kvstore/store.go`):
```go
type Command struct {
    Op       Operation
    Key      string
    Value    string
    ID       string   // 操作追跡用ID
    ClientID string   // クライアント識別子 (重複検知用)
    SeqNum   int      // シーケンス番号 (重複検知用)
}
```

**クライアントセッション管理** (`kvstore/store.go`):
```go
type ClientSession struct {
    LastSeqNum int    // 最後に実行したシーケンス番号
    LastResult Result // キャッシュされた結果
}

type KVStore struct {
    ...
    sessions  map[string]*ClientSession // ClientID -> Session
    sessionMu sync.RWMutex
    ...
}
```

**重複検知ロジック** (`kvstore/store.go:executeCommand`, `:384-400`):
- `cmd.ClientID != ""` の場合のみ重複チェックを実行（`:386`）。**ClientID が空のリクエストは dedup 対象外**で、そのまま実行されます
- `SeqNum < LastSeqNum`: 古いリクエスト → エラー返却
- `SeqNum == LastSeqNum`: 重複リクエスト → キャッシュから結果返却
- `SeqNum > LastSeqNum`: 新しいリクエスト → 実行してセッション更新

**クライアントライブラリ** (`kvstore/client.go`):
- 起動時にユニークなClientIDを生成
- 各操作でシーケンス番号をインクリメント
- Put/Delete操作でClientIDとSeqNumを送信

**スナップショット永続化** (`persistence/kv_snapshotter.go`):
- SnapshotterV2インターフェースでセッション情報も永続化
- 後方互換性あり（V1形式のスナップショットも読み込み可能）

上記の重複検知機構は kvstore 内に実装され、**実際の HTTP API 経路にも配線されました**:

- **重複検知が配線済み (D4・✅ 解消)**: HTTP ハンドラ（PUT/DELETE）が ClientID/SeqNum を読み取り、`executeCommand` の dedup（`cmd.ClientID != ""` で発動）に渡すようになりました。修正 commit `52afd48`。see ../KNOWN_ISSUES.md (D4)。ただし dedup が効くのは **ClientID を指定したリクエストに限られる**条件付きの at-most-once であり、無条件の保証ではありません
- **結果受領後の spurious エラーを解消 (D5・✅ 解消)**: 適用成功後に "leadership lost" を返して不要リトライを誘発する経路はなくなりました。修正 commit `16a9b31`。see ../KNOWN_ISSUES.md (D5)。ただし結果の解決は log index ではなく `opID`（`kvstore/store.go:583`、`<nodeID>-<UnixNano>`）ベースで、`raft.Start` 呼び出し（`:598`）と `pendingOps` への登録（`:611-614`）の間に committed→applied が完了すると結果が握りつぶされ、実際には成功した操作が client 側で timeout 扱いになりえます（R9）
- クライアント側（`kvstore/client.go`）は `seqNum` を採番した後に送信 mutex を解放するため（`:80-91`）、並行呼び出しの到着順を保証しません。また timeout やネットワークエラー時に操作が実際に適用されたかどうかを呼び出し元に伝える契約がありません（R10）
- `Client.Batch`（`kvstore/client.go:216-240`）が送る `POST /kv/batch` に対応するサーバー側ルートは存在せず、`/kv/` prefix ハンドラで空 PUT として処理され `success:true` を返します。batch は実装されていません（R11）

このため、論文 Section 8 の重複検知要件は「ClientID を指定した場合の at-most-once」という条件付きで実 API 経路に配線されています。読み取りは §8 の linearizability を ReadIndex で満たします（上記「8. 読み取り専用クエリ最適化」）。本プロジェクトは教育用途であり、R9〜R11 が残る点は変わりません。

---

### 8. 読み取り専用クエリ最適化 ✅ ReadIndex で線形化

#### 論文の要件
- リーダーは読み取りクエリをログ複製なしで処理可能
- ただし、リーダーシップ確認が必要:
  - **ReadIndex 方式**（採用）: 現在の commit index を readIndex として捕捉し、ハートビート 1 巡で過半数から current-term ACK を集めて「自分がまだ唯一のリーダー」であることを証明してから、readIndex まで適用済みの状態を読む（Raft 学位論文 §6.4）
  - リース方式（不採用）: ハートビート成功から一定時間内は安全とみなす。クロック前提に依存するため撤去済み

#### 実装状況
**ファイル**: `raft/readindex.go`, `raft/noop.go`, `raft/node.go`, `kvstore/store.go`

**ReadIndex プロトコルを実装**。以前はリースベースの読み取り最適化（`CanServeReadOnlyQuery` / `lastLeaderConfirmation`）でしたが、linearizability 違反（D1〜D3）のため **commit `b3b21a4` でリース機構を完全撤去し、commit `60fd631` で当選時 no-op + ReadIndex を導入**しました。現在の流れ:

1. 当選したリーダーは `becomeLeader`（`raft/noop.go`）で current-term の no-op を追記し、それがコミットされると §6.4 の前提（current term で 1 件コミット済み）を満たす
2. `ReadIndex`（`raft/readindex.go`）が次を行い readIndex を返す: ①リーダーかつ current-term コミット済みを確認（未達なら `ErrNoCurrentTermCommit`）②現在の commitIndex を readIndex として捕捉 ③ハートビート 1 巡（`confirmLeadership`）で過半数から current-term ACK を集めてリーダーシップを証明。単一ノードは自分が過半数なので即返す
3. KV 側 `Get`（`kvstore/store.go`）は `ReadIndex()` → `waitForApplied(readIndex)`（`lastAppliedIndex` が readIndex に追いつくまで 2ms 間隔でポーリング、`operationTimeout=5s`）→ `getLocal(key)` の順で線形化読みを行う

```go
// kvstore/store.go - Get は ReadIndex 経由で線形化読み
func (kvs *KVStore) Get(key string) (string, error) {
    readIndex, err := kvs.raft.ReadIndex() // §6.4: 過半数で現リーダーを証明し readIndex を捕捉
    if err != nil {
        return "", err // 非リーダーは ErrNotLeader（"not leader"）→ HTTP 503 リダイレクト
    }
    if err := kvs.waitForApplied(readIndex); err != nil {
        return "", err
    }
    return kvs.getLocal(key) // readIndex まで適用済みのローカル状態から読む
}
```

no-op エントリは適用ループで実行スキップされますが `lastAppliedIndex` は前進させます（`isNoOpCommand`、`kvstore/store.go`）。これにより `waitForApplied` の追い付き判定と log-compaction の会計が no-op を含めて正しく進みます。

**安全性（D1〜D3 解消）**:
- リース方式にあったクロック依存・過半数喪失時の step-down 欠如がなくなりました。孤立した旧リーダーは `confirmLeadership` が過半数 ACK を得られず `ErrLeadershipNotConfirmed` を返し、stale 値を返しません。より高い term を見たら `stepDown` します。D1/D2 解消（commit `b3b21a4`）。see ../KNOWN_ISSUES.md (D1, D2)
- 当選時 no-op（`becomeLeader`）により、新リーダーは前任 term のコミット済みエントリを advance してから読みを許可します。D3 解消（commit `60fd631`）。see ../KNOWN_ISSUES.md (D3)

**残る性質（安全性の穴ではない）**: 選挙直後、no-op がコミットされるまでの短時間は `ErrNoCurrentTermCommit` を返します。これは安全のための待ちであり、レイテンシ上の性質です。読み取りは線形化されますが、本プロジェクトは教育用途であり、グループ R の未修正項目（R9–R16, R18）が残る点は変わりません。

---

## 実装優先度の推奨

2026-07-07 報告書に基づく修正はすべて完了した（下記 1–4）。現在の推奨順は
2026-09-06 再監査（`docs/raft-audit-2026-09-06.md` §6）のロードマップに従う。詳細な優先度・行番号は
`KNOWN_ISSUES.md` の「グループ R」および「修正の推奨順序」を参照。

### 2026-07-07 報告書分（解消済み）
1. ✅ **AppendEntries step 3 の一致確認**（解消済み） — 既存エントリと index/term が一致する場合は切り詰めない conflict ベース切り詰めを実装（B2・commit `7151e77`）
2. ✅ **ログコンパクション（グループ A）** — 絶対/相対インデックスの統一・投票/コミット/適用経路・`raft.Snapshotter` の本番配線・フォロワー側スナップショット永続化（A1–A6, A8・`8ad5367`/`d0cbdc1`/`c516f54`）に加え、受信側の §7 保持ルール（A7・`019d33e`）も解消。**グループ A に未修正項目なし**（同じ経路に 2026-09-06 再監査が見つけた R3–R5 も解消済み）
3. ✅ **当選時 no-op の導入とリース設計の見直し**（解消済み） — 当選時 no-op（`becomeLeader`、commit `60fd631`）と ReadIndex 方式（`b3b21a4`）を実装し、旧リース機構を撤去（D1〜D3）
4. ✅ **重複検知の実配線**（解消済み・条件付き） — HTTP API 経路への ClientID/SeqNum の受け渡し（D4・commit `52afd48`）と、"leadership lost" spurious エラーの解消（D5・commit `16a9b31`）を実装。ただし dedup は ClientID 指定時のみで、opID ベースの結果解決には R9 が残る

### 2026-09-06 再監査分（`docs/raft-audit-2026-09-06.md` §6 のロードマップ順）
1. ✅ 完了 — 現状訂正のみ（PR #22）
2. ✅ 完了 — CI を `go test -race ./...` に拡大し、`tests/integration/cluster_test.go` の
   timeout `break` を修正（R17、`19bdb37`/`91b0f7c`）
3. ✅ 完了 — R1（`Start` の leader 判定と append の原子化、`2c26b9a`）、R2（未永続 merge のロールバックによる durable ACK、`c362ae4`）
4. ✅ 完了 — R3（世代整合・順序不変条件・起動時検証、`0695b95`）、R4（payload/metadata の同一世代化、`f53617e`）、R5（古い snapshot の適用禁止、`156510a`）
5. ✅ 完了 — R6（`becomeFollowerLocked` への降格統一、`ac93fcb`）、E1（選挙タイマーの mutex 統一、`c5fdc0f`）、E2（送信エントリのコピー、`7e3eb61`）、peer 単位の送信直列化と `MatchIndex`/`NextIndex` の単調化（`499c4b8`）
6. ✅ 完了 — B3 の ordered applier（`f873d9b`）と shutdown lifecycle（R19: `Kill` が自分の起動した goroutine を join し、`main.go` は HTTP API → transport → Kill → `kvs.Close()` の順で停止。`7c96f14` / `13570d5`）
7. R9–R13（KV/client/API/RPC 細部） — P1
8. R15（chunk transfer） — P2
9. R14（joint consensus: state → quorum → 管理 API → snapshot の順） — P2
10. R16、R18（設定、examples/benchmark） — P3

---

## 参照

- [Raft論文 (PDF)](https://raft.github.io/raft.pdf)
- [Raft Visualization](https://raft.github.io/)
- [Diego Ongaro's PhD Dissertation](https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf) - より詳細な仕様
