# gowetype

从输入法的私有数据目录里导出**个人词库**和**剪贴板历史**，可以按词、拼音、词频筛选。

纯 Go，零第三方依赖，能交叉编译成 Android arm64 直接在手机上跑。所有读取都是只读的，
不打开数据库也不改任何文件，所以输入法正在运行时也能安全读。

## 安装

从 [Releases](https://github.com/YuleBest/gowetype/releases) 下载对应平台的二进制，
或者自己编译：

```bash
make            # 本机
make android    # Android arm64
make install    # 编译并推到 /data/local/tmp/gowetype
```

## 用法

```
gowetype words        # 导出个人词库
gowetype grep         # 筛选词条
gowetype clipboard    # 导出剪贴板历史
gowetype list         # 列出探测到的数据库，不导出
```

不指定包名时会自动探测已安装的输入法。读 `/data` 需要 root，没有 root 就先把数据
目录拷出来，用 `-data` 指过去。

### words

```bash
gowetype words -format txt -o /sdcard/Download/words.txt
gowetype words -min 2 -max 4          # 只看两到四个汉字的词
gowetype words -no-hot                # 不合并热词库
gowetype words -length                # 只输出词的数量
```

### grep

位置参数是词的模式，可以给多个，之间是「或」。`-pinyin` 是拼音的模式，两边也是
「或」关系。匹配方式有四种：

| 方式 | 说明 |
| --- | --- |
| 默认 | 子串匹配，忽略大小写 |
| `-exact` | 整串相等 |
| `-re` | 正则，Go 的 regexp |
| `-re -i` | 正则且忽略大小写 |

正则用的是 RE2 语法，不支持反向引用和环视。匹配是「查找」语义，要整串匹配就写
`^...$`。

```bash
gowetype grep 报错 目录                  # 词里含"报错"或"目录"
gowetype grep -re '^[a-zA-Z]站$'         # 匹配到 B站
gowetype grep -re '^[\x{4e00}-\x{9fff}]{2}$'   # 正好两个汉字
gowetype grep -re -pinyin '^wei,zhuang$'       # 拼音正好是 wei,zhuang
gowetype grep -re -pinyin '^(wei|bao),' -score-min 20
gowetype grep -v -re '^[\x{4e00}-\x{9fff}]{6,}$'   # 排除六个字以上的
gowetype grep -score-min 40 -score-max 60
gowetype grep 报错 -length
```

`-pinyin` 可以重复给，多个之间是「或」。不能用逗号分隔多个模式，因为拼音本身含逗号。

参数和模式可以混着写，`grep 报错 -length` 和 `grep -length 报错` 效果一样。

### clipboard

```bash
gowetype clipboard -limit 100
gowetype clipboard -format tsv -cols createTime,content -o cb.tsv
gowetype clipboard -format txt        # 只输出正文
gowetype clipboard -length            # 只输出条数
```

### 通用参数

| 参数 | 说明 |
| --- | --- |
| `-pkg NAME` | 应用包名，默认自动探测 |
| `-data DIR` | 直接指定应用数据目录，跳过包名探测 |
| `-o FILE` | 输出到文件，默认写 stdout |
| `-format` | words 和 grep 支持 tsv、txt、json，clipboard 支持 json、tsv、txt |
| `-length` | 只输出数量，不受 `-limit` 影响 |

## 输出格式

`words` 和 `grep` 的 tsv 是三列：词、拼音、词频，按词频降序。

```
文件资源管理器	wen,jian,zi,yuan,guan,li,qi	608419
伪装	wei,zhuang	51
```

词频只有部分词条有值，没有的显示 0。txt 是每行一个词，适合当词表喂给别的输入法。

## 支持的输入法

微信输入法验证得最完整，个人词库和剪贴板都能导。小布输入法能导剪贴板。

识别方式是看数据特征而不是写死路径，所以换输入法或者官方改了目录结构一般也能用。
装了多个输入法时，可以先用 `gowetype list -pkg <包名>` 看看有什么。

## 常见问题

**提示读不到数据目录。** 需要 root。或者把数据目录拷出来用 `-data` 指定。

**词频为什么大多是 0。** 微信输入法的词库有两类记录，只有其中一类带词频。

**能不能导入到别的输入法。** 这个工具只负责导出。小布输入法没有可用的文件导入接口，
它的词库包是私有格式并且由原生引擎校验，没法直接写。

## 开发

数据格式和解析原理见 [FORMAT.md](FORMAT.md)。

## 许可

MIT
