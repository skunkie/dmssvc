### A Windows service for [dms](https://github.com/anacrolix/dms)

How to build a binary:

```
GOOS=windows GOARCH=amd64 go build -trimpath -buildmode=pie -ldflags="-s -w" -o dmssvc.exe
```

How to create a Windows service:

```
sc.exe create dmssvc binPath= "c:\path\to\dmssvc.exe" start= auto
```

To configure the service, use [dmssvc.json](./dmssvc.json). Put it in the same directory.