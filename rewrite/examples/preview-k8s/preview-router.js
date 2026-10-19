// The router's check of the signed preview URLs (README.md, "The signed
// URL"), for nginx's njs module. router.yaml.example loads it with
//   js_import preview from preview-router.js;
// and calls:
//   open           /_preview/open?t=<end>.<signature>: after the password
//                  check, a valid token makes the cookie __Host-preview and
//                  a 302 to /; anything else is 403. The cookie's value is
//                  <end>.<the cookie's own signature>, never the URL's token:
//                  whoever reads the URL still needs the password to hold one.
//   check          the auth_request of every other path: 204 when the cookie
//                  holds a valid cookie value for this host, 403 otherwise
//                  (the URL's token sent as the cookie included).
//   otherCookies   the Cookie header without this router's cookie, for the
//                  application: it never sees the token.
//   filterCookies  removes from the application's answer every Set-Cookie of
//                  the router's own name, and every one whose Domain
//                  attribute (any of them, in any letter case, with or
//                  without spaces around "=") names something other than
//                  this host. nginx's proxy_cookie_domain has rewritten the
//                  Domain to the host before; this catches what it left.
//   relayKey       the key the API relay adds (README.md, "Relaying an API"),
//                  from the environment variable PREVIEW_RELAY_KEY; empty
//                  when there is none, and the relay then sends nothing.
// The key is the environment variable PREVIEW_SIGNING_KEY of the nginx
// process (nginx.conf says "env PREVIEW_SIGNING_KEY;"). Without a key of 32
// characters or more every token is refused. Nothing here writes a token to
// a log, and no function throws on what a request sends.
import crypto from 'crypto';

const COOKIE = '__Host-preview';
const MINIMUM_KEY = 32;
const TOKEN = /^([0-9]{1,12})\.([A-Za-z0-9_-]{43})$/;

function key() {
    var value = process.env.PREVIEW_SIGNING_KEY || '';
    return value.length >= MINIMUM_KEY ? value : '';
}

// HMAC-SHA256 over "<host>\n<end>", base64url without "=" padding: what
// preview.py's sign() writes.
function sign(secret, host, end) {
    return crypto.createHmac('sha256', secret).update(host + '\n' + end).digest('base64url');
}

// The cookie's own signature: the same, over "cookie\n<host>\n<end>". Only
// open() makes it, after the password and a valid token.
function signCookie(secret, host, end) {
    return crypto.createHmac('sha256', secret).update('cookie\n' + host + '\n' + end).digest('base64url');
}

// Compares two strings without stopping at the first difference.
function same(a, b) {
    if (typeof a !== 'string' || typeof b !== 'string' || a.length !== b.length) {
        return false;
    }
    var difference = 0;
    for (var i = 0; i < a.length; i++) {
        difference |= a.charCodeAt(i) ^ b.charCodeAt(i);
    }
    return difference === 0;
}

// The end of a value that is valid for this host now, or 0. The signature
// is made again (by signer) from the host the request names ($host:
// lowercase, no port) and the end as written, then compared; only then is
// the end read as a number.
function checked(signer, value, host, now) {
    var secret = key();
    if (!secret || typeof value !== 'string' || typeof host !== 'string' || !host) {
        return 0;
    }
    var parts = TOKEN.exec(value);
    if (!parts || !same(signer(secret, host, parts[1]), parts[2])) {
        return 0;
    }
    var end = Number(parts[1]);
    return end > now ? end : 0;
}

// A URL's token.
function valid(token, host, now) {
    return checked(sign, token, host, now);
}

// A cookie's value.
function validCookie(value, host, now) {
    return checked(signCookie, value, host, now);
}

function now() {
    return Math.floor(Date.now() / 1000);
}

function cookies(header) {
    if (Array.isArray(header)) {
        header = header.join('; ');
    }
    return typeof header === 'string' && header ? header.split(';') : [];
}

function own(entry) {
    return entry.split('=', 1)[0].trim() === COOKIE;
}

function cookieToken(r) {
    var found = cookies(r.headersIn.Cookie).filter(own);
    if (found.length !== 1) {
        return '';
    }
    var entry = found[0];
    return entry.slice(entry.indexOf('=') + 1).trim();
}

function open(r) {
    var token = r.args && typeof r.args.t === 'string' ? r.args.t : '';
    var moment = now();
    var end = valid(token, r.variables.host, moment);
    if (!end) {
        r.return(403);
        return;
    }
    var value = end + '.' + signCookie(key(), r.variables.host, String(end));
    r.headersOut['Set-Cookie'] = [COOKIE + '=' + value + '; Path=/; Max-Age=' + (end - moment)
        + '; Secure; HttpOnly; SameSite=Lax'];
    r.headersOut['Referrer-Policy'] = 'no-referrer';
    r.headersOut['Cache-Control'] = 'no-store';
    r.return(302, '/');
}

function check(r) {
    r.return(validCookie(cookieToken(r), r.variables.host, now()) ? 204 : 403);
}

function otherCookies(r) {
    return cookies(r.headersIn.Cookie).filter(function (entry) {
        return entry.trim() && !own(entry);
    }).map(function (entry) {
        return entry.trim();
    }).join('; ');
}

// Whether every Domain attribute of a Set-Cookie value names this host. A
// cookie without one stays with the host that set it.
function hostOnly(entry, host) {
    var attributes = entry.split(';').slice(1);
    for (var i = 0; i < attributes.length; i++) {
        var at = attributes[i].indexOf('=');
        var name = (at < 0 ? attributes[i] : attributes[i].slice(0, at)).trim().toLowerCase();
        if (name !== 'domain') {
            continue;
        }
        var value = at < 0 ? '' : attributes[i].slice(at + 1).trim().toLowerCase();
        if (value.charAt(0) === '.') {
            value = value.slice(1);
        }
        if (value !== host) {
            return false;
        }
    }
    return true;
}

function filterCookies(r) {
    var set = r.headersOut['Set-Cookie'];
    if (!set) {
        return;
    }
    var host = String(r.variables.host || '').toLowerCase();
    var kept = (Array.isArray(set) ? set : [set]).filter(function (entry) {
        return !own(entry) && hostOnly(entry, host);
    });
    if (kept.length) {
        r.headersOut['Set-Cookie'] = kept;
    } else {
        delete r.headersOut['Set-Cookie'];
    }
}

function relayKey(r) {
    return process.env.PREVIEW_RELAY_KEY || '';
}

export default {open, check, otherCookies, filterCookies, relayKey, sign, signCookie, valid, validCookie, same};
