package scan

// wordlist.go is the fallback vocabulary the scanners use when the caller
// brings no list of their own.
//
// The embedded list is about INTEREST, not coverage. A pentest that sweeps a
// thousand generic names finds a thousand 404s and reads nothing; a sweep of
// two hundred paths that leak credentials, expose an admin panel or hand out a
// source map finds the things worth reporting. So the list is ordered by how
// often the path is worth a request, and it deliberately carries the file
// names that end careers: .env, .git/config, wp-config.php.bak, id_rsa.
//
// It is a const string rather than a package-level slice because the package
// holds no mutable state. A shared slice handed out by DefaultWordlist could
// be reordered by one caller and quietly changed for the next one.

import (
	"fmt"
	"os"
	"strings"
)

// maxWordlistEntries bounds a loaded list. A wordlist is an input, and an
// input with no bound is a way to spend an operator's budget on a file
// somebody dropped in the session workdir.
const maxWordlistEntries = 10000

// DefaultWordlist returns the embedded fallback list: admin panels, API
// paths, config and backup files, version control artefacts, cloud metadata
// paths and the framework endpoints that leak. The returned slice is a copy,
// so a caller that sorts or truncates it cannot change what the next caller
// gets.
func DefaultWordlist() []string {
	return normalizeWordlist(strings.Split(defaultWordlistText, "\n"))
}

// LoadWordlist reads a wordlist file, one entry per line. Blank lines and
// lines starting with # are skipped, entries are trimmed and deduplicated,
// and the result is bounded at 10000 entries.
//
// An empty path returns the embedded default, so a caller that has no list
// and a caller that asked for the default end up in the same place.
func LoadWordlist(path string) ([]string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return DefaultWordlist(), nil
	}
	data, err := os.ReadFile(trimmed)
	if err != nil {
		// The path is named in the error because a retry has to be able to
		// correct it, and a wordlist the model just wrote lives under a
		// directory it may have spelled differently.
		return nil, fmt.Errorf("read wordlist %s: %w", trimmed, err)
	}
	return normalizeWordlist(strings.Split(string(data), "\n")), nil
}

// normalizeWordlist trims, drops blanks and comments, deduplicates and
// bounds a list of raw lines.
func normalizeWordlist(lines []string) []string {
	seen := make(map[string]bool, len(lines))
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		entry := strings.TrimSpace(line)
		if entry == "" || strings.HasPrefix(entry, "#") {
			continue
		}
		if seen[entry] {
			continue
		}
		seen[entry] = true
		out = append(out, entry)
		if len(out) >= maxWordlistEntries {
			break
		}
	}
	return out
}

// defaultWordlistText is the embedded list. One entry per line; a line
// starting with # is a comment and is skipped by the same rule the file
// loader uses.
const defaultWordlistText = `
# Admin panels and consoles
admin
adminpanel
admin-panel
admin_area
adminarea
admincp
admin_login
admin_panel
administration
administrator
adminer
adminer.php
admins
admin-console
admin1
admin2
adm
panel
panels
controlpanel
control-panel
cpanel
webadmin
manage
manager
management
dashboard
dash
console
backend
backoffice
back-office
siteadmin
sysadmin
superadmin
control
cp

# API surfaces
api
api/v1
api/v2
api/v3
api/docs
api-docs
apidocs
api-info
api-key
api-keys
api_key
apiadmin
api/config
api/debug
api/health
api/metrics
api/spec
api/status
api/users
api/version
internal/api
_api
api.php
graphql
graphiql
graphql/console
gql
grpc
grpc-web
rest
restapi
rest-api
rpc
xmlrpc
jsonrpc
json-rpc
service
services
soap
wsdl
openapi.json
openapi.yaml
swagger
swagger.json
swagger.yaml
swagger-ui
swagger-ui.html
v1
v2
v3
gateway
proxy

# Auth, sessions and identity
login
login.php
login.html
signin
sign-in
sign_in
signin.php
logout
logout.php
register
signup
sign-up
auth
authenticate
authentication
authorization
oauth
oauth2
oauth/authorize
oauth/token
sso
saml
cas
callback
callbacks
webhook
webhooks
hooks
token
tokens
jwt
session
sessions
credentials
credential
secrets
secret
password
passwords
reset
reset.php
forgot
forgot-password
change-password
apikey
apikeys
private
key
keys
user
users
account
accounts
profile
profiles
me
self

# Config, environment and backup files
.env
.env.local
.env.production
.env.development
.env.backup
.env.bak
.env.old
.env.example
env
env.js
env.json
config
config.js
config.json
config.yml
config.yaml
config.xml
config.ini
config.php
config.bak
config.old
config.php.bak
config.php~
config.php.old
config.php.save
config.php.swp
config.php.orig
configuration
configuration.php
settings
settings.py
settings.js
settings.json
settings.yml
settings.ini
settings.xml
settings.php
settings.bak
web.config
app.config
appsettings.json
appsettings.Production.json
wp-config.php.bak
wp-config.bak
wp-config.php~
wp-config.php.old
wp-config.php.save
wp-config.php.swp
wp-config.php.orig
wp-config.php.txt
backup
backup.zip
backup.sql
backup.tar
backup.tar.gz
backup.bak
backup.old
backups
db-backup
dump
dump.sql
dump.sql.gz
dump.zip
db.sql
database.sql
database.sql.gz
data.sql
site.sql
mysql.sql
sql.sql
www.zip
site.zip
old.zip
old.tar.gz
archive.zip
archive.tar.gz
export.sql
users.sql
orders.sql
structure.sql
data.tar.gz

# Version control and CI
.git/config
.git/HEAD
.git/index
.gitignore
.gitattributes
.git/packed-refs
.git/refs/heads/master
.git/hooks/pre-commit
.svn/entries
.svn/wc.db
.hg/requires
.hg/hgrc
.bzr/README
CVS/Root
.github/workflows
.circleci/config.yml
.travis.yml
Jenkinsfile
.gitlab-ci.yml
azure-pipelines.yml
bitbucket-pipelines.yml
.gitpod.yml
Vagrantfile
playbook.yml

# Cloud, container and infrastructure
.aws/credentials
.aws/config
.ssh/id_rsa
.ssh/authorized_keys
.ssh/known_hosts
.ssh/config
.docker/config.json
.dockerignore
Dockerfile
docker-compose.yml
docker-compose.yaml
docker-compose.override.yml
.kube/config
.terraform/terraform.tfstate
terraform.tfstate
terraform.tfvars
.terraformrc
nginx.conf
httpd.conf
server.xml
web.xml

# Keys and private material
id_rsa
id_rsa.pub
id_dsa
id_ecdsa
id_ed25519
authorized_keys
known_hosts
.netrc
.npmrc
.pypirc
.bash_history
.bashrc
.profile

# Dependency and build manifests
composer.json
composer.lock
composer.phar
package.json
package-lock.json
yarn.lock
pnpm-lock.yaml
npm-shrinkwrap.json
node_modules
vendor
bower_components
Gemfile
Gemfile.lock
requirements.txt
Pipfile
Pipfile.lock
pyproject.toml
setup.py
setup.cfg
manage.py
Cargo.toml
Cargo.lock
go.mod
go.sum
Makefile
Procfile
tsconfig.json
jsconfig.json
webpack.config.js
webpack.config.mjs
next.config.js
next.config.mjs
nuxt.config.js
vite.config.js
vite.config.ts
rollup.config.js
babel.config.js
jest.config.js
.eslintrc
.eslintrc.json
.prettierrc
.babelrc
postcss.config.js
tailwind.config.js
gulpfile.js
Gruntfile.js

# Server, runtime and framework endpoints
server-status
server-info
server-info.php
server-status.php
jmx-console
admin-server
manager/html
manager/status
status
info
health
healthcheck
healthz
livez
readyz
ping
metrics
prometheus
monitoring
actuator
actuator/health
actuator/env
actuator/info
actuator/beans
actuator/mappings
actuator/configprops
actuator/metrics
actuator/heapdump
actuator/threaddump
actuator/loggers
actuator/logfile
actuator/trace
actuator/auditevents
actuator/prometheus
actuator/scheduledtasks
actuator/sessions
actuator/httptrace
actuator/jolokia
actuator/flyway
actuator/liquibase
actuator/shutdown
manage/health
manage/info
manage/env
jolokia
phpinfo
phpinfo.php
phpinfo.php5
phpinfo.php7
info.php
test.php
opcache
opcache.php
apc.php
memcache.php
redis-status.php
fpm-ping
fpm-status
fpm-ping.php
fpm-status.php
elmah.axd
trace
debug
debug/pprof
_debug
_profiler
_status
_error
_cat
_cat/indices
_cat/health
_cat/nodes
_cat/shards
_cat/aliases
_cluster/health
_nodes
_search
_aliases
_mapping
_bulk
_refresh
_stats
_template
_snapshot
_license
_xpack
_plugins
_tasks
_ingest
_gallery
_spring
_solr
_druid
druid
graphiql-playground
_postman
_elastic
_telescope
horizon
nova

# Content management and frameworks
wp-admin
wp-login.php
wp-content
wp-includes
wp-cron.php
wp-json
wp-config.php
wp-config
xmlrpc.php
phpmyadmin
phpMyAdmin
pma
myadmin
mysql
dbadmin
pgadmin
sql
database
db
sites/default
typo3
umbraco
craft
sitecore
silverstripe
openatlas
moodle
joomla
drupal
wordpress
laravel
symfony
symfony/web/app.php
app_dev.php
app.php
admin/api
admin/index.php
admin/login.php
admin/dashboard
admin/config
admin/settings
admin.php
admin.html

# Metadata, robots and well-known
crossdomain.xml
sitemap.xml
sitemap
robots.txt
humans.txt
security.txt
.well-known
.well-known/security.txt
.well-known/openid-configuration
.well-known/oauth-authorization-server
.well-known/change-password
.well-known/assetlinks.json
.well-known/apple-app-site-association
.well-known/host-meta
.well-known/webfinger

# Scripts, includes and uploads
cgi-bin
cgi
scripts
includes
include
inc
src
source
lib
libs
upload
uploads
images
static
assets
public
resources
dist
build
tmp
temp
log
logs
error_log
access_log
debug.log
app.log
application.log
error.log
server.log
.htaccess
.htpasswd
.DS_Store
Thumbs.db
thumbs.db
desktop.ini
WEB-INF
WEB-INF/web.xml
META-INF

# Installers, documentation and generic values
install
installer
install.php
setup
setup.php
readme
README
README.md
readme.txt
README.txt
LICENSE
CHANGELOG
TODO
INSTALL
CONTRIBUTING
INSTALL.md
Caddyfile
guest
root
demo
default
1
0
true
false
null
none
-1
`
