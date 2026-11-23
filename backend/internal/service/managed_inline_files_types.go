package service

import "strings"

// Official Excel add-in build 1795069380; x-square-ZBHnzmnJ.js SHA-256
// 49d756dc73e0bfa67f55dc64d3cd48220ceafaddeade9f5bbf0d15bd7fabb127.
// Lists use the add-in's MIME-or-extension policy. Local file size limits are separate.

// IDe, zero-based Unicode offset 1879020.
var managedInlineFileMIMEs = managedInlineTypeSet(`
application/csv application/epub+zip application/graphql application/haansofthwp
application/haansofthwpx application/hwp+zip application/javascript application/json
application/json5 application/msword application/pdf application/rtf
application/toml application/typescript application/vnd.apple.iwork application/vnd.apple.keynote
application/vnd.apple.pages application/vnd.google-apps.document application/vnd.google-apps.presentation application/vnd.google-apps.spreadsheet
application/vnd.hancom.hwpx application/vnd.hp-hpgl application/vnd.ms-excel application/vnd.ms-outlook
application/vnd.ms-powerpoint application/vnd.ms-powerpoint.presentation.macroenabled.12 application/vnd.ms-powerpoint.slideshow.macroenabled.12 application/vnd.ms-powerpoint.template.macroenabled.12
application/vnd.oasis.opendocument.presentation application/vnd.oasis.opendocument.presentation-template application/vnd.oasis.opendocument.spreadsheet application/vnd.oasis.opendocument.spreadsheet-template
application/vnd.oasis.opendocument.text application/vnd.openxmlformats-officedocument.presentationml.presentation application/vnd.openxmlformats-officedocument.presentationml.slideshow application/vnd.openxmlformats-officedocument.presentationml.template
application/vnd.openxmlformats-officedocument.spreadsheetml.sheet application/vnd.openxmlformats-officedocument.wordprocessingml.document application/x-awk application/x-bash
application/x-graphql application/x-httpd-php application/x-httpd-php-source application/x-hwp
application/x-hwp+zip application/x-hwp-v5 application/x-iif application/x-json5
application/x-ndjson application/x-patch application/x-php application/x-powershell
application/x-protobuf application/x-rust application/x-scala application/x-sql
application/x-subrip application/x-terraform application/x-toml application/x-wine-extension-ini
application/x-yaml application/yaml image/svg+xml message/rfc822
text/calendar text/css text/csv text/gettext-translation
text/html text/javascript text/jsx text/markdown
text/plain text/rtf text/srt text/tab-separated-values
text/tsv text/tsx text/vbscript text/vtt
text/x-algol68 text/x-apache-conf text/x-asm text/x-astro
text/x-awk text/x-bash text/x-c text/x-c++
text/x-clojure text/x-cmake text/x-csharp text/x-dart
text/x-diff text/x-dockerfile text/x-ejs text/x-elixir
text/x-erb text/x-erlang text/x-fortran text/x-go
text/x-golang text/x-gradle text/x-graphql text/x-groovy
text/x-handlebars text/x-handlebars-template text/x-haskell text/x-hcl
text/x-iif text/x-ini text/x-jade text/x-java
text/x-jinja-template text/x-jinja2 text/x-jinja2-template text/x-julia
text/x-kotlin text/x-less text/x-liquid text/x-lisp
text/x-lua text/x-makefile text/x-msdos-batch text/x-mustache
text/x-objectivec text/x-objectivec++ text/x-patch text/x-perl
text/x-php text/x-prolog text/x-properties text/x-protobuf
text/x-pug text/x-python text/x-r text/x-rst
text/x-ruby text/x-rust text/x-sass text/x-scala
text/x-script.python text/x-scss text/x-sh text/x-shellscript
text/x-sql text/x-subrip text/x-swift text/x-terraform
text/x-tex text/x-tmpl text/x-toml text/x-twig
text/x-typescript text/x-vcard text/x-yaml text/x-zsh
text/xml
`)

// LDe, zero-based Unicode offset 1882528.
var managedInlineFileExtensions = managedInlineTypeSet(`
.art .bat .brf .c
.cls .css .csv .diff
.doc .docx .dot .eml
.epub .es .h .hs
.htm .html .hwp .hwpx
.ics .ifb .java .js
.json .keynote .ksh .ltx
.mail .markdown .md .mht
.mhtml .mjs .msg .nws
.odp .ods .odt .otp
.ots .pages .patch .pdf
.pl .pm .pot .potm
.potx .ppa .pps .ppsm
.ppsx .ppt .pptm .pptx
.pwz .py .rst .rtf
.scala .sh .shtml .srt
.sty .svg .svgz .tex
.text .tsv .txt .vcf
.vtt .wiz .xla .xlb
.xlc .xlm .xls .xlsx
.xlt .xlw .xml .yaml
.yml
`)

// pgr, zero-based Unicode offset 1885685.
var managedInlineImageMIMEs = managedInlineTypeSet(`
image/gif image/jpeg image/png image/webp
`)

// mgr, zero-based Unicode offset 1885741.
var managedInlineImageExtensions = managedInlineTypeSet(`
.gif .jpeg .jpg .png
.webp
`)

// _gr, zero-based Unicode offset 1885795.
var managedInlineZIPContainers = managedInlineTypeSet(`
.docm .docx .dotm .dotx
.hwpx .odp .ods .odt
.potm .potx .ppsm .ppsx
.pptm .pptx .xlsm .xlsx
.xltm .xltx
`)

func managedInlineTypeSet(values string) map[string]bool {
	set := make(map[string]bool)
	for _, value := range strings.Fields(values) {
		set[value] = true
	}
	return set
}
