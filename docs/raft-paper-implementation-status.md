# Raft論文実装状況比較

> 最終検証: 2026-07-22 / 対象 commit `3fe5a84`

本ドキュメントは [Raft論文](https://raft.github.io/raft.pdf) の内容と rosetta プロジェクトの実装状況を比較したものです。本プロジェクトは学習目的の実装であり、既知の安全性違反は `KNOWN_ISSUES.md`（および `docs/safety-review-2026-07-07.md`）に集約されています。

## 概要サマリー

| カテゴリ | 状態 | 備考 |
|---------|------|------|
| リーダー選挙 (Section 5.2) | ⚠️ ほぼ準拠 | 基本動作は実装済み。当選時に current-term no-op を追記（D3 解消）。圧縮後の投票判定も絶対 index 化済み（A2 解消・commit `8ad5367`）。残る懸念は E1（ロック外 `ResetElectionTimer` の data race） |
| ログ複製 (Section 5.3) | ✅ 実装済み（fast rollback 含む） | step 3 の conflict ベース切り詰め（B2 解消・commit `7151e77`）、受信側の圧縮後 index 対応（A1 解消・commit `8ad5367`）も完了 |
| 安全性保証 (Section 5.4) | ⚠️ A7 が残る | 選挙制限は実装済み。B2・A2 は解消済み（`7151e77` / `8ad5367`）。残る違反経路は A7（InstallSnapshot 受信側の Log Matching 違反）のみ |
| 永続化 (Figure 2) | ✅ 概ね実装済み | RPC 応答前の persist 規律あり（C1/C2/C4 解消・commit `2a35ce9`）。リーダー自身の追記経路（`AppendLogEntry`/`TruncateLogAfter`）は persist エラーを無視（C3 部分修正） |
| ログコンパクション (Section 7) | ⚠️ A7 が残る（他は解消） | 絶対 index 統一・投票/コミット/適用経路・本番配線・フォロワー側永続化を解消（A1–A6, A8・commit `8ad5367`/`d0cbdc1`/`c516f54`）。残る安全性課題は A7 のみ（`MaxRaftState=0` 推奨は A7 が残る限り維持） |
| クラスタメンバーシップ変更 (Section 6) | ❌ 未実装 | Joint consensus未対応 |
| クライアント相互作用 (Section 8) | ✅ 配線済み（at-most-once） | 重複検知（ClientID/SeqNum）を実 API 経路へ配線し at-most-once 化（D4 解消・commit `52afd48`）。コミット済みは log index で解決（D5 解消・`16a9b31`） |
| 読み取り専用クエリ最適化 | ✅ 線形化実装 | ReadIndex プロトコル + 当選時 no-op で linearizable read を実装。旧リース方式は撤去（D1〜D3 解消） |

（A1〜E2 の ID は see ../KNOWN_ISSUES.md を参照）

> **現在の未修正（安全性）**: A7（InstallSnapshot 受信側の Log Matching 違反）・B3（InstallSnapshot が rs.mu 保持のまま applyCh へブロッキング送信）・E1（ロック外 `ResetElectionTimer` の data race）・E2（送信エントリの backing array をロック外 marshal 中に書き換えうる data race）。加えて C3 は部分修正（リーダー自身の `AppendLogEntry`/`TruncateLogAfter` が persist エラーを無視）。本プロジェクトは教育用途であり、本番運用可ではない。

---

## 詳細比較

### 1. リーダー選挙 (Section 5.2) ⚠️ ほぼ準拠

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
- **ロック外の `ResetElectionTimer` 呼び出し**（`raft/rpc.go:210,225`）による data race。see ../KNOWN_ISSUES.md (E1)

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

// raft/state.go:67-70 - リーダー状態
type LeaderState struct {
    NextIndex  map[string]int  // 各フォロワーへの次送信インデックス
    MatchIndex map[string]int  // 各フォロワーの複製済みインデックス
}
```

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

### 3. 安全性保証 (Section 5.4) ⚠️ A7 が残る

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

選挙制限そのものと、現 term のエントリのみをコミットカウントする §5.4.2 規則（`updateCommitIndex`, `raft/rpc.go:529-531`）は実装済みです。ただし各安全性特性の実際の成立状況は以下の通りです:

- **選挙安全性**: 投票の応答前 persist（commit `2a35ce9`）により、クラッシュ跨ぎの二重投票は防止されます
- **リーダー追記のみ**: リーダーの通常経路では満たされます
- **ログ一致 (Log Matching)**: AppendEntries の切り詰めは §5.3 step 3 準拠に修正済み（B2 解消・`7151e77`）。残る違反経路は InstallSnapshot 受信側が分岐 suffix を term 検査なしで保持する A7 のみ。see ../KNOWN_ISSUES.md (A7)
- **リーダー完全性 (Leader Completeness)**: 選挙制限が絶対 index でスナップショットメタデータを考慮するようになり（A2 解消・commit `8ad5367`）、圧縮後もコミット済みエントリを持たないノードは当選しません。加えて当選時 no-op の追記（`becomeLeader`、D3 解消、commit `60fd631`）により、前任 term のコミット済みエントリは選挙直後に advance されます。
- **状態機械安全性**: 上記が発火しない限り成立

---

### 4. 永続化 (Figure 2) ✅ 概ね実装済み

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
- `RequestVote`: term/投票の変更を persist してから応答。persist 失敗時は投票を取り消す（`raft/rpc.go:101-106`）
- `AppendEntries`: term 変更・追記エントリを persist してから `Success=true` を返す。失敗時は `Success=false`（`raft/rpc.go:136-142`, `:179-182`）
- `startElection`: 立候補（term+1・自己投票）を persist できなければ選挙を中止（`raft/rpc.go:200-212`）
- 永続状態のロード失敗時は起動を拒否（`raft/state.go:157-161`, `raft/node.go:29-34`）

**残課題（C3 部分修正）**: RPC 応答経路の persist エラーは修正済み（`2a35ce9`）ですが、リーダー自身のログ追記 `AppendLogEntry`（Start() 経由の本番経路）と `TruncateLogAfter`（`raft/log.go`）は persist 失敗をログ出力のみで握り潰すため、未永続のエントリがリーダー自票込みでコミットされうる経路が残っています。see ../KNOWN_ISSUES.md (C3)

---

### 5. ログコンパクション / スナップショット (Section 7) ⚠️ A7 が残る（他は解消）

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
}
```

RPC 構造体・スナップショット取得（`TakeSnapshot`/`TruncateLogTo`）・リーダー送信側のインデックス変換に加え、受信・投票・コミット・適用の各経路と本番配線・フォロワー側永続化も解消されました。**残る安全性課題は A7（InstallSnapshot 受信側の Log Matching 違反）のみ**です:

- **受信ハンドラの圧縮後 index 対応 (A1・✅ 解消)**: `AppendEntries` ハンドラは `PrevLogIndex` を `LastIncludedIndex` オフセットに合わせて絶対 index として扱うようになりました。修正 commit `8ad5367`
- **投票経路の絶対 index 化 (A2・✅ 解消)**: `RequestVote`/`startElection` が `LastIncludedIndex/Term` を含む絶対 index で候補者ログの新しさを評価します。修正 commit `8ad5367`
- **リーダー当選時 NextIndex の絶対 seed (A3・✅ 解消)**: `initializeLeaderState` が NextIndex を絶対 index で seed します。修正 commit `8ad5367`
- **コミット判定の絶対 index 化 (A4・✅ 解消)**: `updateCommitIndex` が絶対 last index でコミットを前進させ、リーダー圧縮後もコミットが停止しません。修正 commit `8ad5367`
- **再起動時の LastApplied 復元 (A5・✅ 解消)**: `loadPersistentState` が CommitIndex/LastApplied をスナップショットから復元し、範囲外ガードも入りました。二重適用・範囲外 panic は解消されています。修正 commit `8ad5367`
- **本番構成で InstallSnapshot が配線済み (A6・✅ 解消)**: `RaftSnapshotter` を `SetSnapshotter` で本番配線し、型アサーションでインターフェース互換を担保、V2 形式もパースします。修正 commit `d0cbdc1` + `c516f54`
- **受信側が分岐 suffix を term 検査なしで保持 (A7・❌ 未修正)**: `InstallSnapshot` ハンドラは `LastIncludedIndex` 超のエントリを無条件保持し、論文 §7 の「最終エントリと index/term 両方一致なら以降を保持、さもなくば全破棄」に違反します（`raft/rpc.go` の InstallSnapshot ハンドラ）。see ../KNOWN_ISSUES.md (A7)
- **フォロワー側スナップショットの永続化 (A8・✅ 解消)**: `installSnapshotFromApplyMsg` が `saveSnapshot` でフォロワー側スナップショットをディスクに永続化します。修正 commit `c516f54`

see ../KNOWN_ISSUES.md (A1〜A8)。A7 が残る限り、当面の安全な運用は `MaxRaftState=0`（圧縮無効）です。

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

❌ **実装が必要**

---

### 7. クライアント相互作用 (Section 8) ✅ 配線済み

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

**重複検知ロジック** (`kvstore/store.go:executeCommand`):
- ClientIDが設定されている場合、重複チェックを実行
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

- **重複検知が配線済み (D4・✅ 解消)**: HTTP ハンドラ（PUT/DELETE）が ClientID/SeqNum を読み取り、`executeCommand` の dedup（`cmd.ClientID != ""` で発動）に渡すようになりました。タイムアウト後のリトライは at-most-once 化されています。修正 commit `52afd48`。see ../KNOWN_ISSUES.md (D4)
- **結果受領後の spurious エラーを解消 (D5・✅ 解消)**: コミット済みの操作は log index で解決し、ロール変化後も結果を返すようになったため、適用成功後に "leadership lost" を返して不要リトライを誘発することはなくなりました。修正 commit `16a9b31`。see ../KNOWN_ISSUES.md (D5)

このため、論文 Section 8 の重複検知（at-most-once）要件は実 API 経路で満たされるようになりました。読み取りは §8 の linearizability を ReadIndex で満たします（上記「8. 読み取り専用クエリ最適化」）。ただし本プロジェクトは教育用途であり、A7・B3・C3（部分）・E1・E2 が残る点は変わりません。

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

**残る性質（安全性の穴ではない）**: 選挙直後、no-op がコミットされるまでの短時間は `ErrNoCurrentTermCommit` を返します。これは安全のための待ちであり、レイテンシ上の性質です。読み取りは線形化されますが、本プロジェクトは教育用途であり、他グループ（A7・B3・ログ圧縮まわり等）の未修正項目が残る点は変わりません。

---

## 実装優先度の推奨

修正の詳細な推奨順序は `docs/safety-review-2026-07-07.md` を参照。概要:

### 高優先度（安全性違反の解消）
1. ✅ **AppendEntries step 3 の一致確認**（解消済み） — 既存エントリと index/term が一致する場合は切り詰めない conflict ベース切り詰めを実装（B2・commit `7151e77`）
2. **ログコンパクションの残課題** — 絶対/相対インデックスの統一・投票/コミット/適用経路・`raft.Snapshotter` の本番配線・フォロワー側スナップショット永続化は解消済み（A1–A6, A8・commit `8ad5367`/`d0cbdc1`/`c516f54`）。**残るは A7（InstallSnapshot 受信側の Log Matching 違反）のみ**。A7 が残る限り暫定策は `MaxRaftState=0` で圧縮無効化
3. ✅ **当選時 no-op の導入とリース設計の見直し**（解消済み） — 当選時 no-op（`becomeLeader`、commit `60fd631`）と ReadIndex 方式（`b3b21a4`）を実装し、旧リース機構を撤去（D1〜D3）
4. ✅ **重複検知の実配線**（解消済み） — HTTP API 経路への ClientID/SeqNum の受け渡し（D4・commit `52afd48`）と、結果受領時の正しい応答処理（D5・commit `16a9b31`）を実装

### 低優先度
5. **クラスタメンバーシップ変更**
   - Joint Consensus実装
   - 運用中のクラスタ拡張/縮小が必要な場合のみ

---

## 参照

- [Raft論文 (PDF)](https://raft.github.io/raft.pdf)
- [Raft Visualization](https://raft.github.io/)
- [Diego Ongaro's PhD Dissertation](https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf) - より詳細な仕様
