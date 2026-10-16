package backup

import (
	"path"
	"strings"
)

// compressedExts lists formats whose content is already compressed (or
// encrypted), so deflating them again costs CPU without saving space.
// Used by compression "auto".
var compressedExts = map[string]bool{}

func init() {
	for _, e := range strings.Fields(`
		jpg jpeg png gif webp heic heif avif jxl cr2 cr3 nef arw dng
		mp4 m4v mov mkv avi wmv flv webm mpg mpeg m2ts 3gp hevc
		mp3 m4a aac ogg oga opus flac wma ape alac
		zip 7z rar gz tgz bz2 tbz2 xz txz zst lz4 lzma br z cab arj lzh
		dmg pkg apk aab ipa xapk jar war ear deb rpm msi msix appx snap whl nupkg crx xpi
		docx xlsx pptx odt ods odp epub pages numbers vsdx
		pdf woff woff2 gpg pgp
	`) {
		compressedExts[e] = true
	}
}

// isCompressedExt reports whether the file name has a known compressed format.
func isCompressedExt(name string) bool {
	ext := strings.TrimPrefix(path.Ext(name), ".")
	return ext != "" && compressedExts[strings.ToLower(ext)]
}
