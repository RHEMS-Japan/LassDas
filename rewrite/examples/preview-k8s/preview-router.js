// The router's check of the signed preview URLs (README.md, "The signed
// URL"), for nginx's njs module. router.yaml.example loads it with
//   js_import preview from preview-router.js;
// and calls:
//   open           /_preview/open?t=<end>.<signature>: after the password
//                  check, a valid token becomes the cookie __Host-preview and
//                  a 302 to /; anything else is 403.
//   check          the auth_request of every other path: 204 when the cookie
//                  holds a valid token for this host, 403 otherwise.
//   otherCookies   the Cookie header without this router's cookie, for the
//                  application: it never sees the token.
//   dropOwnCookie  removes a Set-Cookie of the same name from the
//                  application's answer.
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

// The end of a token that is valid for this host now, or 0. The signature is
// made again from the host the request names ($host: lowercase, no port)
// and the end as written, then compared; only then is the end read as a
// number.
function valid(token, host, now) {
    var secret = key();
    if (!secret || typeof token !== 'string' || typeof host !== 'string' || !host) {
        return 0;
    }
    var parts = TOKEN.exec(token);
    if (!parts || !same(sign(secret, host, parts[1]), parts[2])) {
        return 0;
    }
    var end = Number(parts[1]);
    return end > now ? end : 0;
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
    r.headersOut['Set-Cookie'] = [COOKIE + '=' + token + '; Path=/; Max-Age=' + (end - moment)
        + '; Secure; HttpOnly; SameSite=Lax'];
    r.headersOut['Referrer-Policy'] = 'no-referrer';
    r.headersOut['Cache-Control'] = 'no-store';
    r.return(302, '/');
}

function check(r) {
    r.return(valid(cookieToken(r), r.variables.host, now()) ? 204 : 403);
}

function otherCookies(r) {
    return cookies(r.headersIn.Cookie).filter(function (entry) {
        return entry.trim() && !own(entry);
    }).map(function (entry) {
        return entry.trim();
    }).join('; ');
}

function dropOwnCookie(r) {
    var set = r.headersOut['Set-Cookie'];
    if (!set) {
        return;
    }
    var kept = (Array.isArray(set) ? set : [set]).filter(function (entry) {
        return !own(entry);
    });
    if (kept.length) {
        r.headersOut['Set-Cookie'] = kept;
    } else {
        delete r.headersOut['Set-Cookie'];
    }
}

export default {open, check, otherCookies, dropOwnCookie, sign, valid, same};
