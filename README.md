# Namakani

A webmail client that aggregates multiple mailboxes for reading and writing in one place. Supports the IMAP and SMTP protocols, and connecting to Proton and Tuta accounts.

## Build and run

Requires Go 1.26 and Node 22.

```sh
cd web && npm install && npm run build && cd ..
go build -o namakani .
./namakani -addr <address>:<port> -data <directory>
```

## License

GPL v3, see LICENSE. Third-party licenses are in THIRD-PARTY-NOTICES.TXT.
