# Namakani

A webmail client that aggregates multiple mailboxes for reading and writing in one place. Supports the IMAP and SMTP protocols, and connecting to Proton and Tuta accounts.

## Build and run

Requires Go 1.26 and Node 22.

```sh
cd web && npm install && npm run build && cd ..
go build -o namakani .
./namakani
```

Listens on 127.0.0.1:8080 by default, with data in ./data. Both can be changed with -addr and -data.

## License

GPL v3, see LICENSE. Third-party licenses are in THIRD-PARTY-NOTICES.TXT.
