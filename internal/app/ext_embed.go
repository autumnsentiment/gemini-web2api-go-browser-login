package app

import (
	"archive/zip"
	"bytes"
	"embed"
	"io/fs"
)

// 浏览器扩展源码内嵌。
//
// 远程浏览器场景（用户自己电脑上的 Chrome/Edge）需要把扩展发给用户安装。
// 源码在 internal/app/ext_assets/（从 tools/cookie-sync/ext-src 同步而来），
// 打进二进制后由 /admin/api/browser/extension 打包成 zip 下载。
//
//go:embed ext_assets/*
var extFS embed.FS

func extEmbedded() bool {
	f, err := extFS.Open("ext_assets/manifest.json")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// buildExtZip 把内嵌的扩展目录打包成 zip 字节。
func buildExtZip() ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := fs.WalkDir(extFS, "ext_assets", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := path[len("ext_assets/"):]
		data, err := extFS.ReadFile(path)
		if err != nil {
			return err
		}
		w, err := zw.Create(rel)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	})
	if err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
