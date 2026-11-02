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

`SecretExpiry` returns when the configured client secret expires, so a service can warn before mail stops working. See below for how it finds that secret.

## Permissions

The app registration needs the `Mail.Send` application permission for the `From` mailbox. `SecretExpiry` also needs `Application.Read.All`. Both need admin consent.

## Behaviour

`Send` makes up to four attempts on 429, 500, 502, 503, 504 and network errors, including failed token requests. Between attempts it waits the number of seconds in `Retry-After`, or 1, 2 and 4 seconds when there is none. It stops when Graph asks to wait longer than 30 seconds. A retry after a lost response can deliver a mail twice, so this favours delivery over avoiding duplicates.

HTTP requests time out after 30 seconds. Token requests do not follow the context of `Send`, so a cancelled send can still wait for a token request to finish.

Graph shows only the first three characters of each secret. `SecretExpiry` returns the earliest expiry of the unexpired secrets that start with the same three characters as the configured one.

## License

MIT
