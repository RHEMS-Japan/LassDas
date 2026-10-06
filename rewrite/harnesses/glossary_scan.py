"""Look for the glossary's words to avoid in the documents it covers.

rewrite/GLOSSARY.md gives one word for each thing the engine handles and, for
each, the words not to use for it. This reads those lists and looks for the
words in rewrite/README.md, START.md and RUNTIME.md, the repository's
top-level CLAUDE.md and the `instructions` texts of rewrite/examples/*.json,
and in any further file named on the command line. A further file whose name
ends in .json is read for its `instructions` texts only; any other is read
whole.

It also checks the glossary against the same documents: every English and
Japanese word of an entry is used in them, every file that an entry lists
after "Used in" uses one of the entry's words, no word to avoid is also a
word the glossary chooses, and every word listed under "Waiting for
rewording" still appears, so that the list only holds words that a rewording
has yet to remove. The glossary's own text, outside its lists and quoted
words, is read for the words to avoid as well.

Usage: glossary_scan.py [--root REPOSITORY] [FILE ...]

Exit status: 0 when every check passes; 1 when a word to avoid appears outside
the waiting list or another check fails; 2 when the glossary or a document
cannot be read as described.
"""
import argparse
import bisect
import json
from pathlib import Path
import re
import sys

GLOSSARY = "rewrite/GLOSSARY.md"
DOCUMENTS = ("rewrite/README.md", "rewrite/START.md", "rewrite/RUNTIME.md", "CLAUDE.md")
EXAMPLES = "rewrite/examples"
WAITING = "Waiting for rewording"

SECTION = re.compile(r"^## (.+?)\s*$")
HEADING = re.compile(r"^### (.+?)\s*$")
USED = re.compile(r"^- Used in: (.+?)\s*$")
AVOID = re.compile(r"^- Avoid: (.+?)\s*$")
ITEM = re.compile(r"^`([^`]+)`(?: except in (`[^`]+`(?:, `[^`]+`)*))?$")
REFERENCE = re.compile(r"^(\S+):([1-9][0-9]*)$")
WAITING_ITEM = re.compile(r"^- `([^`]+)`")
QUOTED = re.compile(r"`[^`\n]*`")
INSTRUCTIONS_KEY = re.compile(r'\s*"instructions"\s*:\s*')


class Unreadable(Exception):
    """The glossary or a document is not in the form this scan reads."""


def pattern(word):
    """An English word matches as a whole word in any letter case, and also
    with s or es added; a space matches any run of blanks, line ends included.
    Any other word matches exactly as written."""
    if not word.isascii():
        return re.compile(re.escape(word))
    body = r"\s+".join(re.escape(part) for part in word.split())
    if word[0].isalnum():
        body = r"(?<![A-Za-z0-9_])" + body
    if word[-1].isalnum():
        body += r"(?:e?s)?(?![A-Za-z0-9_])"
    return re.compile(body, re.IGNORECASE)


class Text:
    """One text the scan reads, with the line each of its offsets is on."""

    def __init__(self, name, body, first_line=1, one_line=False):
        self.name = name
        self.body = body
        self.first_line = first_line
        self.one_line = one_line
        self.breaks = [index for index, character in enumerate(body) if character == "\n"]

    def line(self, offset):
        if self.one_line:
            return self.first_line
        return self.first_line + bisect.bisect_left(self.breaks, offset)


class Avoid:
    def __init__(self, word, exceptions, owner, line):
        self.word = word
        self.exceptions = exceptions
        self.owner = owner
        self.line = line
        self.pattern = pattern(word)
        self.exception_patterns = [pattern(exception) for exception in exceptions]

    def places(self, text):
        """Offsets in text where the word appears outside its exceptions."""
        excepted = [(found.start(), found.end())
                    for exception in self.exception_patterns
                    for found in exception.finditer(text.body)]
        return [found.start() for found in self.pattern.finditer(text.body)
                if not any(start <= found.start() and found.end() <= end
                           for start, end in excepted)]


class Entry:
    def __init__(self, english, japanese, line):
        self.english = english
        self.japanese = japanese
        self.line = line
        self.used = []
        self.words = [word for word in (english, japanese) if word]

    def label(self):
        return self.english + (" / " + self.japanese if self.japanese else "")


def read_text(path, name):
    try:
        return path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError) as error:
        raise Unreadable("%s cannot be read: %s" % (name, error))


def read_glossary(path):
    entries, avoids, waiting = [], [], []
    section = entry = None
    fenced = False
    prose = []
    for number, line in enumerate(read_text(path, "GLOSSARY.md").splitlines(), 1):
        if line.startswith("```"):
            fenced = not fenced
            prose.append("")
            continue
        if fenced:
            prose.append("")
            continue
        found = SECTION.match(line)
        if found:
            section, entry = found.group(1), None
            prose.append(line)
            continue
        found = HEADING.match(line)
        if found:
            english, _, japanese = found.group(1).partition(" / ")
            entry = Entry(english.strip(), japanese.strip() or None, number)
            entries.append(entry)
            prose.append(line)
            continue
        found = USED.match(line)
        if found:
            if entry is None:
                raise Unreadable("GLOSSARY.md:%d: Used in outside an entry" % number)
            for reference in found.group(1).split(", "):
                place = REFERENCE.match(reference.strip())
                if not place:
                    raise Unreadable("GLOSSARY.md:%d: %r is not FILE:LINE" % (number, reference))
                entry.used.append((place.group(1), int(place.group(2))))
            prose.append(line)
            continue
        found = AVOID.match(line)
        if found:
            owner = entry.label() if entry else section
            if owner is None or section == WAITING:
                raise Unreadable("GLOSSARY.md:%d: Avoid outside an entry or section" % number)
            listed = found.group(1)
            if listed != "none.":
                for item in listed.split("; "):
                    parts = ITEM.match(item)
                    if not parts:
                        raise Unreadable("GLOSSARY.md:%d: cannot read the word to avoid %r" % (number, item))
                    exceptions = re.findall(r"`([^`]+)`", parts.group(2) or "")
                    avoids.append(Avoid(parts.group(1), exceptions, owner, number))
            prose.append("")
            continue
        if section == WAITING and line.startswith("- "):
            listed = WAITING_ITEM.match(line)
            if not listed:
                raise Unreadable("GLOSSARY.md:%d: cannot read the waiting word in %r" % (number, line))
            waiting.append((listed.group(1), number))
            prose.append("")
            continue
        prose.append(line)
    if fenced:
        raise Unreadable("GLOSSARY.md: a code fence is not closed")
    if not entries or not avoids:
        raise Unreadable("GLOSSARY.md: no entries or no words to avoid")
    own = Text("GLOSSARY.md", QUOTED.sub(" ", "\n".join(prose)))
    return entries, avoids, waiting, own


def instruction_values(value):
    if isinstance(value, dict):
        for key, inner in value.items():
            if key == "instructions" and isinstance(inner, str):
                yield inner
            yield from instruction_values(inner)
    elif isinstance(value, list):
        for inner in value:
            yield from instruction_values(inner)


def instructions(path, name):
    """Each instructions text of a JSON file, with the line it is on."""
    raw = read_text(path, name)
    try:
        expected = sorted(instruction_values(json.loads(raw)))
    except ValueError as error:
        raise Unreadable("%s is not JSON: %s" % (name, error))
    found = []
    decoder = json.JSONDecoder()
    for number, line in enumerate(raw.split("\n"), 1):
        key = INSTRUCTIONS_KEY.match(line)
        if not key:
            continue
        try:
            value, _ = decoder.raw_decode(line, key.end())
        except ValueError:
            continue
        if isinstance(value, str):
            found.append(Text(name, value, number, one_line=True))
    if sorted(text.body for text in found) != expected:
        raise Unreadable("%s: an instructions text does not start on a line of its own, "
                         "so its line cannot be named" % name)
    return found


def shown(relative):
    """The name a document has from the glossary's own directory."""
    return relative[len("rewrite/"):] if relative.startswith("rewrite/") else "../" + relative


def documents(root):
    texts = []
    for relative in DOCUMENTS:
        name = shown(relative)
        texts.append(Text(name, read_text(root / relative, name)))
    examples = sorted((root / EXAMPLES).glob("*.json"))
    if not examples:
        raise Unreadable("no examples under %s" % EXAMPLES)
    for path in examples:
        texts.extend(instructions(path, shown(EXAMPLES + "/" + path.name)))
    return texts


def further(paths):
    texts = []
    for given in paths:
        path = Path(given)
        if path.suffix == ".json":
            texts.extend(instructions(path, given))
        else:
            texts.append(Text(given, read_text(path, given)))
    return texts


def lines_of(places):
    """README.md:3,7; START.md:2 for [(name, line), ...] in their order."""
    grouped = {}
    for name, line in places:
        grouped.setdefault(name, [])
        if line not in grouped[name]:
            grouped[name].append(line)
    return "; ".join("%s:%s" % (name, ",".join(str(line) for line in lines))
                     for name, lines in grouped.items())


def check(root, paths):
    entries, avoids, waiting, own = read_glossary(root / GLOSSARY)
    texts = documents(root)
    extra = further(paths)
    failures, notes = [], []

    known = {avoid.word for avoid in avoids}
    words = [avoid.word for avoid in avoids]
    for word in sorted({word for word in words if words.count(word) > 1}):
        failures.append("GLOSSARY.md: %s is listed as a word to avoid more than once" % word)
    waiting_words = [word for word, _ in waiting]
    for word in sorted({word for word in waiting_words if waiting_words.count(word) > 1}):
        failures.append("GLOSSARY.md: %s is listed under %s more than once" % (word, WAITING))
    for word, line in waiting:
        if word not in known:
            failures.append("GLOSSARY.md:%d: %s is waiting for rewording but is not a word to avoid"
                            % (line, word))

    for avoid in avoids:
        for entry in entries:
            for chosen in entry.words:
                if avoid.places(Text("", chosen)):
                    failures.append("GLOSSARY.md:%d: %s is a word to avoid and part of the entry %s"
                                    % (avoid.line, avoid.word, entry.label()))

    waiting_places = {word: [] for word in waiting_words}
    found_outside = 0
    for avoid in avoids:
        for text in texts + extra:
            for offset in avoid.places(text):
                place = (text.name, text.line(offset))
                if avoid.word in waiting_places and text in texts:
                    waiting_places[avoid.word].append(place)
                else:
                    found_outside += 1
                    failures.append("%s:%d: %s is a word to avoid (%s)"
                                    % (place[0], place[1], avoid.word, avoid.owner))
        for offset in avoid.places(own):
            failures.append("GLOSSARY.md:%d: the glossary itself uses %s (%s)"
                            % (own.line(offset), avoid.word, avoid.owner))
    for word, line in waiting:
        if word in known and not waiting_places.get(word):
            failures.append("GLOSSARY.md:%d: %s no longer appears; remove it from %s"
                            % (line, word, WAITING))

    by_name = {}
    for text in texts:
        by_name.setdefault(text.name, []).append(text)
    for entry in entries:
        patterns = [pattern(word) for word in entry.words]
        for word, matcher in zip(entry.words, patterns):
            if not any(matcher.search(text.body) for text in texts):
                failures.append("GLOSSARY.md:%d: %s is not used in the documents" % (entry.line, word))
        for name, line in entry.used:
            if name not in by_name:
                raise Unreadable("GLOSSARY.md:%d: %s is not one of the documents the scan reads"
                                 % (entry.line, name))
            lines = sorted({text.line(found.start()) for text in by_name[name]
                            for matcher in patterns for found in matcher.finditer(text.body)})
            if not lines:
                failures.append("GLOSSARY.md:%d: %s no longer uses %s" % (entry.line, name, entry.label()))
            elif line not in lines:
                nearest = min(lines, key=lambda candidate: abs(candidate - line))
                notes.append("note: %s:%d named for %s does not use it now; the nearest use is %s:%d"
                             % (name, line, entry.label(), name, nearest))

    for word, _ in waiting:
        if waiting_places.get(word):
            print("waiting for rewording: %s at %s" % (word, lines_of(waiting_places[word])))
    for note in notes:
        print(note)
    for failure in failures:
        print(failure)
    print("glossary: %d entries, %d words to avoid; %d found outside the waiting list, "
          "%d waiting for rewording in %d places"
          % (len(entries), len(avoids), found_outside, len(waiting),
             sum(len(places) for places in waiting_places.values())))
    print("result: " + ("fail" if failures else "pass"))
    return 1 if failures else 0


def main(argv=None):
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(errors="backslashreplace")
    parser = argparse.ArgumentParser(description="Look for the glossary's words to avoid.")
    parser.add_argument("--root", default=str(Path(__file__).resolve().parents[2]),
                        help="the repository's top directory (default: the repository this file is in)")
    parser.add_argument("files", nargs="*", help="further files to read for words to avoid")
    arguments = parser.parse_args(argv)
    try:
        return check(Path(arguments.root), arguments.files)
    except Unreadable as error:
        print("glossary scan cannot run: %s" % error, file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
