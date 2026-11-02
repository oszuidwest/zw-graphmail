# zw-graphmail

Go package that sends mail through Microsoft Graph as an Entra app, using the client credentials flow. It is meant to replace the separate Graph mail code in the ZuidWest Go services.

```sh
go get github.com/oszuidwest/zw-graphmail
```

## Usage

```go
mail, err := graphmail.New(graphmail.Config{
	TenantID:     "...",
	ClientID:     "...",
	ClientSecret: "...",
	From:         "noreply@zuidwest.nl",
})
if err != nil {
	return err
}
err = mail.Send(ctx, &graphmail.Message{
	To:      []string{"techniek@zuidwest.nl"},
	Subject: "Silence on FM",
	Body:    "Since 14:02",
})
```

Set `HTML: true` for an HTML body. `Attachments` takes files as bytes.

`SecretExpiry` returns when the configured client secret expires, so a service can warn before mail stops working.

## Permissions

The app registration needs the `Mail.Send` application permission for the `From` mailbox. `SecretExpiry` also needs `Application.Read.All`. Both need admin consent.

## Behaviour

`Send` makes up to four attempts on 429, 500, 502, 503, 504 and network errors. It waits for `Retry-After` when Graph sends one, and otherwise 1, 2 and 4 seconds. It stops when Graph asks to wait longer than 30 seconds. A retry after a lost response can deliver a mail twice. For alerts that is better than losing one.

A rejected token request, for example a wrong secret, is not retried.

Requests time out after 30 seconds. Token requests do not follow the context of `Send`, so a cancelled send can wait up to that timeout for a token.

Graph shows only the first three characters of each secret. `SecretExpiry` matches the configured secret on those. If more unexpired secrets start with the same three characters, it returns the earliest expiry.

## License

MIT
