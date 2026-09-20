# 数据格式与解析原理

这份文档说明 gowetype 解析了哪些文件、格式长什么样，以及为什么这么做。要改解析
代码或者适配新的输入法时看这里。

## 代码结构

| 文件 | 职责 |
| --- | --- |
| `main.go` | 命令行、参数解析、输出 |
| `discover.go` | 探测应用数据目录和库文件 |
| `leveldb.go` | LevelDB 的 SSTable 与 WAL 读取 |
| `snappy.go` | snappy 解压 |
| `wetype.go` | 微信输入法 userdict 的条目解析 |
| `sqlite.go` | 只读 SQLite 解析 |
| `clipboard.go` | 剪贴板表识别与导出 |

零第三方依赖是刻意的。snappy 和 SQLite 都自己实现，这样 `CGO_ENABLED=0` 就能交叉
编译到 Android，不需要 NDK。

## 探测策略

不写死路径。包名是参数，数据目录按 Android 的几种约定去找：`/data/user/0/<pkg>`、
`/data/data/<pkg>`，再扫 `/data/user/*/<pkg>` 覆盖多用户。

具体哪个文件是库，按内容特征判断：

- LevelDB：目录里有 `CURRENT` 和 `MANIFEST-*`
- SQLite：文件头是 `SQLite format 3`
- 剪贴板表：表名里含 `clipboard`，列名从建表语句里读

装了多个输入法时，会先挑确实有对应数据的那个，而不是按候选表顺序取第一个。

## 微信输入法的数据在哪

```
/data/user/0/com.tencent.wetype/
├── MicroMsg/wxime/common/userdict/
│   ├── db_path.conf            里面写着当前账号的目录名
│   ├── v5/<uin>/               个人词库
│   └── user_hot_word/          热词，格式相同
└── databases/ime_database      剪贴板历史，SQLite
```

`v5/` 下面那串数字不要写死，从 `db_path.conf` 读。

几个容易走错的地方：

| 路径 | 是什么 |
| --- | --- |
| `databases/ime_database` | 剪贴板历史，不是词库 |
| `files/mmkv/wxkb`，约 1 MB | 设置项，key 全是 `ime_*` |
| `files/wetype_tool/dex_cache/wetype_tool.phrase.*` | 自定义短语，整体加密 |
| `MicroMsg/wxime/d/*.bin` | 引擎自带的系统词库，不是个人词库 |

## LevelDB

一个目录里有两类文件：`*.ldb` 是已落盘、按 key 排序的 SSTable，`*.log` 是预写日志。
读的顺序是先按编号升序读所有 `.ldb`，编号越大越新，新值覆盖旧值，再用 `.log` 覆盖
一遍。

工具不打开数据库，直接按格式解析文件。这样即使输入法正在运行、`LOCK` 被别人拿着，
也能安全读取。

### SSTable

从文件尾部往前看：

```
[data block] ... [metaindex] [index] [footer 48B]
```

footer 最后 8 字节是魔数 `0xdb4775248b80fb57`，前面是两组 varint 的 offset 和 size，
分别指向 metaindex 和 index。index block 的每个条目是「分隔 key 指向 BlockHandle」，
顺着它就能把每个 data block 读出来。

每个 block 后面跟 5 字节 trailer：1 字节压缩类型，0 表示不压，1 表示 snappy，再加
4 字节 crc32c。block 内容是前缀压缩的条目加一个 restart 偏移数组：

```
条目 = varint(shared) + varint(nonShared) + varint(valueLen) + key增量 + value
key  = 上一条 key 的前 shared 字节 + key增量
```

snappy 的 raw block 格式只有 literal 和三种 copy 指令。copy 必须逐字节回拷，因为
LZ77 的 run 依赖源和目标重叠时的展开。

### WAL

`.log` 按 32 KB 分块，每块里是物理记录：

```
crc32c(4B) | length(2B) | type(1B) | payload
```

type 是 FULL、FIRST、MIDDLE、LAST。一条逻辑记录可能跨块，要用 FIRST 到 LAST 拼起来。
拼好的是 WriteBatch：

```
sequence(8B) | count(4B) | 若干条: type(1B) | keyLen(varint) | key | [valueLen(varint) | value]
```

type 为 1 是 PUT，0 是 DELETE。不校验 crc 只顺序读，遇到被截断的尾巴就丢掉，这正好
让工具能在输入法运行时读。

## 微信的 key 前缀

数据库里所有 key 都带前缀，前缀决定记录类型。中文词条主要来自这四个：

| 前缀 | 结构 |
| --- | --- |
| `!u_d_v_p!` | `<拼音>\x01<词>\x01<id>`，分隔干净，最可靠的来源 |
| `!v2u!` | `<拼音>\x01<suffix>`，value 尾部是 u32 词频加 UTF-8 词 |
| `!user_bi` | `gram2!<flag>\x7f<拼音>\x7f<词>\x01<id>`，二元组 |
| `!dcact!` | value 是 `03 + LE16长度 + "拼音\x01词"` |

`!user_pr` 是上下文统计，`!en_fe!*` 是英文词，`!u_e_s!` 是表情，都不导出。

有两个坑。

**`!v2u!` 的词在 value 尾部但没有固定偏移。** value 是「变长二进制头部加 u32 词频
加 UTF-8 词」，不同记录头部差十几字节，算不出偏移。做法是从结尾往前找起点 `i`，
要求 `v[i:]` 是严格合法的 UTF-8、不含控制字符、且含汉字。二进制头部几乎不可能同时
满足这三条。第一版按「往前扫高位字节」来切，结果把 `B站` 切成了 `站`。

**`!v2u!` 的 key 里拼音带上下文。** 比如「吧」的 key 里拼音是 `bai,fen,ba`，前面是
上一个词的拼音。本词拼音是它的后缀，音节数等于词里的汉字数，按字数切末尾即可。
`!u_d_v_p!` 的拼音不带上下文，同一个词优先用它的值。

## SQLite

剪贴板库是 WAL 模式的普通 SQLite。同样不打开数据库，自己解析：

- 文件头 100 字节里有 page size、保留区、文本编码
- page 1 的前 100 字节是文件头，之后才是 b-tree 页头
- b-tree 页头：类型、单元格数、单元格指针数组
- 表叶子页单元格：`varint 负载长度 | varint rowid | 负载`，负载超出页容量时走溢出页
- 记录：`varint 头部长度 | 若干 serial type | 依次排列的值`

### 三个必须处理的地方

**叶子页的单元格指针数组在偏移 8，内部页在偏移 12。** 8 到 11 字节是内部页的最右子
指针，叶子页没有。第一版无条件用了 12，结果 `sqlite_master` 里五张表只读出三张。

**`INTEGER PRIMARY KEY` 是 rowid 的别名。** 这种列在记录里存的是 NULL，真值在
b-tree 的 rowid 上，必须补回去。不补的话 `clipboard_record.id` 整列都是空的。这个
更阴，因为第一版比对脚本恰好漏了 `id` 字段，显示「0 差异」，是靠「id 集合不相等」
和「字段差异 0」自相矛盾才发现的。

**WAL 要自己叠加。** WAL 是 32 字节文件头加若干帧，每帧是 24 字节帧头加一页数据。
帧头里的 salt 必须和文件头一致，校验和是从文件头前 24 字节开始跨帧累加的，只采纳到
最后一个完整提交为止的帧。

校验和的字节序有个坑：magic 最低位是 SQLite 说的 `bigEndCksum`，为 0 表示按本机
字节序，ARM 和 x86 上就是小端，为 1 表示反过来按大端。这里写反会导致校验失败、整份
WAL 被静默丢掉，程序不报错照常输出，只是读的是旧值。表现是 `useTimes` 比 sqlite3
少 1。

## 验证

- 词库：11579 个唯一词，与一份独立的 Python 实现逐条一致
- 剪贴板：500 条记录共 17 列 8500 个字段，与 `sqlite3` 全列比对 0 差异
- 探测：把库放到 `some/deep/nested/dir/` 这种非标准位置也能认出来
- 跨输入法：换 `-pkg` 能直接读小布输入法的库
