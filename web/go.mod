// web is a nested module so the root module's `./...` (build, vet, test) does
// not descend into web/node_modules. It only embeds web/dist (stdlib only); the
// root module imports it via a replace directive in the root go.mod.
module github.com/vkizim/cairn/web

go 1.26
