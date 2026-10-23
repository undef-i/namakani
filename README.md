# Namakani

## Build

Requires Go 1.26 and Node 22.

```sh
cd web && npm install && npm run build && cd ..
go build -o namakani .
```

## Run

```sh
./namakani
```

It listens on 127.0.0.1:8080 and keeps its data in ./data. Two flags change that:

- `-addr` listen address
- `-data` data directory

## License

Namakani is free software, licensed under the GNU General Public License
version 3. See the LICENSE file for the full text. Third-party components
keep their own licenses; they are listed in THIRD-PARTY-NOTICES.TXT.
