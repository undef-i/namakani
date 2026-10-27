# Namakani

把多个邮箱聚合到一起读写的网页邮箱客户端。支持 IMAP、SMTP 协议，支持 Proton、Tuta 邮箱。

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
