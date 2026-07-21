# Raft論文実装状況比較

> 最終検証: 2026-07-21 / 対象 commit `9383cfe`

本ドキュメントは [Raft論文](https://raft.github.io/raft.pdf) の内容と rosetta プロジェクトの実装状況を比較したものです。本プロジェクトは学習目的の実装であり、既知の安全性違反は `KNOWN_ISSUES.md`（および `docs/safety-review-2026-07-07.md`）に集約されています。

## 概要サマリー

| カテゴリ | 状態 | 備考 |
|---------|------|------|
| リーダー選挙 (Section 5.2) | ⚠️ 一部問題あり | 基本動作は実装済み。圧縮後の投票判定がスナップショットを無視 (A2)、当選時 no-op なし (D3) |
| ログ複製 (Section 5.3) | ⚠️ 一部問題あり | Fast rollback最適化含む。ただし step 3 の無条件切り詰め (B2)、圧縮後の受信経路未対応 (A1) |
| 安全性保証 (Section 5.4) | ⚠️ 違反経路あり | 選挙制限は実装済みだが、B2/A2/A7 により Log Matching / Leader Completeness が破れうる |
| 永続化 (Figure 2) | ✅ 概ね実装済み | RPC 応答前の persist 規律あり（commit `2a35ce9`）。リーダー自身の追記経路に残課題 |
| ログコンパクション (Section 7) | ❌ 不完全 | 送信側のみインデックス変換対応。受信・投票・コミット・適用経路が未対応 (A1〜A8) |
| クラスタメンバーシップ変更 (Section 6) | ❌ 未実装 | Joint consensus未対応 |
| クライアント相互作用 (Section 8) | ⚠️ 未配線 | 重複検知の実装はあるが実 API 経路から呼ばれない (D4) |
| 読み取り専用クエリ最適化 | ⚠️ 安全でない | リースベースの読み取りは実装済みだが linearizability 違反経路あり (D1〜D3) |

（A1〜E2 の ID は see ../KNOWN_ISSUES.md を参照）

---

## 詳細比較

### 1. リーダー選挙 (Section 5.2) ⚠️ 一部問題あり

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

状態遷移・ランダム化タイムアウト・過半数当選・投票と任期の応答前 persist（`raft/rpc.go:101-106`、`startElection` の `raft/rpc.go:200-212`）は実装済みです。ただし以下の未達があります:

- **当選時の no-op エントリがない**: 当選処理（`raft/rpc.go:299-305`）はリーダー状態への遷移のみで、論文 Section 8 が要求する就任時 no-op コミットを行いません。前任 term のコミット済みエントリを advance できず、リース読みと組み合わさると ACK 済み書き込みを読み逃します。see ../KNOWN_ISSUES.md (D3)
- **投票判定がスナップショットを無視**: `RequestVote`（`raft/rpc.go:81-84`）と `startElection`（`raft/rpc.go:214-218`）は `len(rs.persistent.Log)` を lastLogIndex として使い、`LastIncludedIndex/Term` を考慮しません。ログ圧縮後は「空ログ」を名乗って投票してしまい、Leader Completeness が破れます。see ../KNOWN_ISSUES.md (A2)
- **ロック外の `ResetElectionTimer` 呼び出し**（`raft/rpc.go:210,225`）による data race。see ../KNOWN_ISSUES.md (E1)

---

### 2. ログ複製 (Section 5.3) ⚠️ 一部問題あり

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

基本の複製フローと fast rollback は実装済みです。ただし以下の論文違反があります:

- **AppendEntries step 3 の無条件切り詰め**: 受信ハンドラ（`raft/rpc.go:165-169`）は `len(args.Entries) > 0` のとき、既存エントリとの conflict 確認なしに `Log[:PrevLogIndex]` へ切り詰めて追記します。論文 §5.3 step 3 は「同一 index で term が異なる場合のみ削除」を要求しており、HTTPトランスポートに順序保証・重複排除がないため、遅延・重複配送された古い AppendEntries がコミット済み suffix を削除しえます。see ../KNOWN_ISSUES.md (B2)
- **受信ハンドラが圧縮後のインデックス体系に未対応**: リーダー送信側（`replicateToPeer`, `raft/rpc.go:350-397`）は絶対/相対インデックス変換を行いますが、受信ハンドラ（`raft/rpc.go:146-186`）は `PrevLogIndex` をそのままスライス位置として使い `LastIncludedIndex` オフセットを考慮しません。圧縮済みフォロワーへの複製はログを破壊します。see ../KNOWN_ISSUES.md (A1)

---

### 3. 安全性保証 (Section 5.4) ⚠️ 違反経路あり

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
- **ログ一致 (Log Matching)**: AppendEntries の無条件切り詰め（B2）と InstallSnapshot の term 検査なし suffix 保持（A7）により破れうる。see ../KNOWN_ISSUES.md (B2, A7)
- **リーダー完全性 (Leader Completeness)**: 選挙制限がスナップショットメタデータを無視するため（A2）、圧縮後はコミット済みエントリを持たないノードが当選しえます。また no-op 欠落（D3）により前任 term のコミット済みエントリの advance が遅延します。see ../KNOWN_ISSUES.md (A2, D3)
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

**残課題**: リーダー自身のログ追記 `AppendLogEntry`（`raft/log.go:136-138`）は persist 失敗をログ出力のみで握り潰すため、未永続のエントリがリーダー自票込みでコミットされうる経路が残っています。

---

### 5. ログコンパクション / スナップショット (Section 7) ❌ 不完全

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

RPC 構造体・スナップショット取得（`TakeSnapshot`/`TruncateLogTo`）・リーダー送信側のインデックス変換（`replicateToPeer`, `raft/rpc.go:350-397`）は実装済みですが、**インデックス体系の変換が送信側にしか実装されておらず、圧縮が一度発火すると通常運転で安全性が破れます**:

- **受信ハンドラが未対応 (A1)**: `AppendEntries` ハンドラは `PrevLogIndex` を `LastIncludedIndex` オフセットなしでスライス位置として使用（`raft/rpc.go:146-186`）
- **投票経路が未対応 (A2)**: `RequestVote`/`startElection` が `GetLastLogIndexWithSnapshot` 等（実装済み: `raft/snapshot.go:195-212`）を使わず `len(Log)` を使用
- **リーダー当選時の NextIndex が相対値 (A3)**: `initializeLeaderState`（`raft/state.go:274`）は `len(Log)+1` を設定するが、送信側はこれを絶対値として解釈
- **コミット判定が未対応 (A4)**: `updateCommitIndex`（`raft/rpc.go:529`）は絶対値の CommitIndex と相対長 `len(Log)` を比較し、リーダー圧縮後にコミットが停止
- **再起動時に LastApplied が復元されない (A5)**: volatile state は常に `{0,0}` で初期化され（`raft/state.go:143`）、`applyEntries`（`raft/log.go:209`）が `Log[LastApplied-1]` を相対位置として使うため、二重適用や範囲外 panic に至る
- **本番構成で InstallSnapshot が配線されていない (A6)**: `RaftNode.SetSnapshotter` の呼び出しは `main.go` に存在せず、`persistence.KVSnapshotter` は `raft.Snapshotter` インターフェース（`raft/snapshot.go:16-27`）を実装していない
- **受信側が分岐 suffix を term 検査なしで保持 (A7)**: `InstallSnapshot` ハンドラ（`raft/rpc.go:624-630`）は `Index > LastIncludedIndex` のエントリを無条件保持。論文 §7 の「最終エントリと index/term 両方一致なら以降を保持、さもなくば全破棄」に違反
- **フォロワー側スナップショットの永続化欠如 (A8)**: `installSnapshotFromApplyMsg`（`kvstore/store.go:303-320`）はメモリ更新のみで KV スナップショットをディスクに保存しない

see ../KNOWN_ISSUES.md (A1〜A8)。当面の安全な運用は `MaxRaftState=0`（圧縮無効）です。

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

### 7. クライアント相互作用 (Section 8) ⚠️ 実装はあるが未配線

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

上記の重複検知機構は kvstore 内に実装されていますが、**実際の HTTP API 経路からは呼ばれません**:

- **重複検知が未配線 (D4)**: HTTP ハンドラ（`main.go:84`）→ `Put(key, value)` → `executeOperationWithResult`（`kvstore/store.go:457-462`）は ClientID/SeqNum を設定せず、opID（nodeID + UnixNano）のみを使います。dedup は `cmd.ClientID != ""` の場合のみ発動する（`kvstore/store.go:323`）ため、実 API 経路ではデッドコードです。クライアントライブラリは ClientID/SeqNum を送信しますが、サーバー側が読み取りません。タイムアウト後のリトライで二重適用が成立します。see ../KNOWN_ISSUES.md (D4)
- **結果受領後の spurious エラー (D5)**: 適用結果を受領済み（=コミット確定済み）にもかかわらず term/leader 再確認で "leadership lost" エラーを返す（`kvstore/store.go:485-489`）ため、不要なリトライを誘発し D4 と組み合わさって二重適用を能動的に生みます。see ../KNOWN_ISSUES.md (D5)

このため、論文 Section 8 の linearizability 要件は現状満たされていません。

---

### 8. 読み取り専用クエリ最適化 ⚠️ 実装済みだが安全でない

#### 論文の要件
- リーダーは読み取りクエリをログ複製なしで処理可能
- ただし、リーダーシップ確認が必要:
  - ハートビート確認: クエリ前に過半数からの応答を確認
  - リース方式: ハートビート成功から一定時間内は安全

#### 実装状況
**ファイル**: `raft/state.go`, `raft/node.go`, `kvstore/store.go`

**リースベースの読み取り最適化を実装**:
- リーダーが過半数からのハートビート応答を受け取った時刻を追跡
- その時刻が選挙タイムアウト以内なら、Raftログを経由せずローカルから直接読み取り

```go
// raft/state.go - リーダーシップ確認の追跡
lastLeaderConfirmation time.Time // For read-only query optimization

// raft/state.go - 読み取り可能かチェック
func (rs *RaftState) CanServeReadOnlyQuery() bool {
    if rs.state != Leader {
        return false
    }
    // 選挙タイムアウト以内にリーダーシップが確認されていれば安全
    return time.Since(rs.lastLeaderConfirmation) < rs.electionTimeout
}

// kvstore/store.go - GETでの最適化利用
func (kvs *KVStore) Get(key string) (string, error) {
    if kvs.raft.CanServeReadOnlyQuery() {
        return kvs.getLocal(key) // ローカルから直接読み取り
    }
    // フォールバック: Raft経由
    return kvs.executeOperationWithResult(OpGet, key, "")
}
```

**ハートビート時の過半数確認** (`replicateToPeer`, `raft/rpc.go:430-434`):
```go
// 成功応答をカウントし、過半数到達でリースを更新
newCount := atomic.AddInt32(successCount, 1)
if int(newCount) >= majority && atomic.CompareAndSwapInt32(leaderConfirmed, 0, 1) {
    rs.lastLeaderConfirmation = time.Now()
}
```

リースベースの読み取り機構自体は実装されていますが、**linearizability を破る経路が確認されています**:

- **リース期間の設計不備 (D1)**: リース期間に自ノードのランダムな election timeout（150-300ms、都度再抽選）を流用しており、より短い timeout を引いたフォロワーがリース有効中に当選できます。クロックドリフト余裕も過半数喪失時の step-down もなく、孤立した旧リーダーが最大 ~150ms のステイル窓で古い値を返します。see ../KNOWN_ISSUES.md (D1)
- **リース起点が応答受信時刻 (D2)**: 正しい起点はハートビート送信時刻であり、応答遅延分リースが不当に延長されます。see ../KNOWN_ISSUES.md (D2)
- **当選時 no-op の欠落 (D3)**: リース成立は commitIndex と無関係なため、新リーダーが前任 term のコミット済みエントリを適用する前にリース読みが成立し、ACK 済みの書き込みを含まない状態を返します。パーティション不要で再現します。see ../KNOWN_ISSUES.md (D3)

厳密な線形化が必要な場合は、リース方式ではなく readIndex 方式（過半数確認 + 適用待ち）が必要です。

---

## 実装優先度の推奨

修正の詳細な推奨順序は `docs/safety-review-2026-07-07.md` を参照。概要:

### 高優先度（安全性違反の解消）
1. **AppendEntries step 3 の一致確認** — 既存エントリと index/term が一致する場合は切り詰めない（B2）
2. **ログコンパクションの全面改修** — 絶対/相対インデックスの統一、投票・コミット・適用経路の対応、`raft.Snapshotter` の実配線、フォロワー側スナップショット永続化（A1〜A8）。暫定策は `MaxRaftState=0` で圧縮無効化
3. **当選時 no-op の導入とリース設計の見直し** — 確実な線形化が必要なら readIndex 方式へ（D1〜D3）
4. **重複検知の実配線** — HTTP API 経路への ClientID/SeqNum の受け渡しと、結果受領時の正しい応答処理（D4/D5）

### 低優先度
5. **クラスタメンバーシップ変更**
   - Joint Consensus実装
   - 運用中のクラスタ拡張/縮小が必要な場合のみ

---

## 参照

- [Raft論文 (PDF)](https://raft.github.io/raft.pdf)
- [Raft Visualization](https://raft.github.io/)
- [Diego Ongaro's PhD Dissertation](https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf) - より詳細な仕様
