# Namakani

把多个邮箱聚合到一起读写的网页邮箱客户端，IMAP、Proton、Tuta 原生接入。Go 单二进制，数据存本地。

## 构建与运行

需要 Go 1.26 与 Node 22。

```sh
cd web && npm install && npm run build && cd ..
go build -o namakani .
./namakani
```

默认监听 127.0.0.1:8080，数据在 ./data，`-addr` 与 `-data` 可改。

## 许可证

GPL v3，见 LICENSE。第三方组件的许可证见 THIRD-PARTY-NOTICES.TXT。
