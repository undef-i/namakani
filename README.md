# Namakani

单用户网页邮箱客户端。一个主密码登录，把多个邮箱聚合进本地一个库统一读写。Go 单二进制，数据存本地。

- 多账号聚合，IMAP、Proton、Tuta 原生接入
- 会话式收件箱
- 发送可撤销
- 定时发送
- 全文搜索
- Telegram 与浏览器推送通知
- 中英文界面

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
