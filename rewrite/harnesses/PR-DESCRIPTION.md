# Keep an implementation explanation in the pull request

An operator can set `DELIVERY_PR_BODY_FILE` to a repository-relative document
path. The fixed delivery process adds that document, unchanged, after its
short description of the delivery. It does not parse a role's answer or
require headings, fields, a verdict, or any special document format.

For example, an operator might choose `docs/change-notes.md`. Tell the work
role where to write the explanation and tell the review role to check it
along with the code. Include the path in the existing allowed write scope
and `DELIVERY_ALLOWED_PATHS` if it will change. Give the delivery process:

```text
DELIVERY_PR_BODY_FILE=docs/change-notes.md
```

The path is an example, not a required filename. This setting neither grants
write access nor expands the delivery scope. With the setting absent, the
existing fixed pull request description is unchanged. Shipped examples do
not select a document automatically; operators must choose a location and
adopt the work/review instructions for their own workflow.

The document can contain the implementation explanation, findings, settings
examples and verification commands the requester needs. It is read from the
commit being delivered, not a later worktree edit. Ordinary committed files
are accepted; paths outside the repository, links and directories are not.
The whole document is checked for the configured forbidden text and the
delivery credential, even if the document itself is unchanged in this round.
These checks do not prove that it contains no other sensitive information;
the existing review still has to examine what will be published.

## Size and errors

The complete body, including the delivery's introductory text, is limited
by this process to 60,000 UTF-8 bytes. This is a conservative process limit,
not a statement of the hosting service's exact limit. Oversized or invalid
UTF-8 text is refused with a reason, never silently shortened or repaired.
For a larger explanation, retain its full text in another reviewed repository
document and make the chosen description document link to it. Such a change
must stay within the existing write and delivery permissions.

Reading or updating a description is not delivery by itself. A failed or
unconfirmed update returns nonzero so the configured recovery can continue;
it does not merge the pull request or undo earlier commits, pushes or remote
operations. A successful description is not evidence that the request has
been satisfied, that it was deployed, or that its explanation is true.

## Later rounds and human changes

While a pull request remains open, another reviewed round updates its body
from the selected document. The delivery record retains the last supplied
text and an outstanding submission so a lost response can be reconciled on
retry. Updates are read back before being accepted. An observed description
that differs from both supplied texts is left alone with a reason; reconcile
that edit with the repository document before continuing. Closed, merged or
human-owned branch handoffs retain their existing behavior.

There is no atomic comparison-and-update here. A person's edit between the
last read and the update can race with this process, and an uncertain HTTP
retry can repeat an update. Coordinate direct edits with the automation;
do not treat the recorded text as a guarantee that no concurrent edit can be
overwritten. A process killed between external operations can also leave a
push or pull request present before its local confirmation is saved.

## Ticket comments

This option changes the pull request body, not the report role's comments.
The report role must still include the requested explanation or a suitable
link and post it through its configured tracker access. It cannot infer
production delivery merely from a pull request or a repository document.
Model summarization and service-side limits remain separate from this
transport. The local tests use synthetic services, not a live account.
