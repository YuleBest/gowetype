BIN     := gowetype
OUT     := $(BIN)-arm64
LDFLAGS := -s -w

.PHONY: all build android install clean

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

android:
	CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT) .

install: android
	adb push $(OUT) /data/local/tmp/$(BIN)
	adb shell chmod 755 /data/local/tmp/$(BIN)
	@echo "手机上跑: adb shell /data/local/tmp/$(BIN)"

clean:
	rm -f $(BIN) $(OUT)
