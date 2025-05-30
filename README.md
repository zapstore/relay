# Relay

The Relay is an app store relay designed for Zapstore. It keeps software events and indexes repositories from GitHub. Also, this relay acts as a community (NIP-CC) relay for Zapstore premium users.


# How to run?

You have to set environment variables defined in [the example file](./.env.example) first on the target system. Then you can build the project using:

```sh
go build .
```

> `make build` will do the same for you.

The you can run the relay using:

```sh
./relay
```

# License

[MIT License](./LICENSE)
