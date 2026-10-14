package llamaserver

import "net/http"

var client = &http.Client{
	Transport: &http.Transport{
		DisableKeepAlives: true,
	},
}
