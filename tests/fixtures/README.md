# Untrusted TLS fixture

`untrusted-cert.pem` is a self-signed certificate for `synthetic.test`.
`untrusted-key.pem` is its deliberately public, test-only private key. Never
use either file for a real service. The browser test must reject this certificate;
it is not installed into any trust store. No external `openssl` command is needed
to run the tests on Windows, macOS, or Linux.
