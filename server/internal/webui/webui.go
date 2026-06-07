package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed dist
var dist embed.FS

func Handler() (http.Handler, error) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, err
	}
	return http.FileServer(http.FS(sub)), nil
}
