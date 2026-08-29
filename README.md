# tdrive

将 Telegram 账号转换为网络驱动器。

## 二进制

```bash
TDRIVE_DATA_DIR=./data ./tdrive
```

## Docker

```bash
docker run -d --name tdrive -p 8080:8080 -v "$(pwd)/data:/data" ghcr.io/dibin666/tdrive:latest
```
