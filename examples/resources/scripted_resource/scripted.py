# ---------------------------------------------------------------------------
#  scripted.py -- helper library for programs run by terraform-provider-scripted
#
#  DO NOT EDIT. This file is vendored: it is a copy of the one shipped with
#  the provider, and local changes will be lost the next time it is refreshed.
#  To update it (or to get the copy matching your provider version), run:
#
#      terraform-provider-scripted --emit-helper > scripted.py
#
#  Provider version this copy came from: see HELPER_VERSION below.
#  Protocol version it speaks:           see PROTOCOL below.
#  Source: https://github.com/audiocodes/terraform-provider-scripted
#          (helpers/python/scripted.py)
#
#  SPDX-License-Identifier: Apache-2.0
#  Licensed under the Apache License, Version 2.0 (the provider's LICENSE),
#  a permissive license: this file may be copied into any project.
# ---------------------------------------------------------------------------
"""Helper for programs run by the Terraform provider `scripted`.

Vendor this file next to your program (it needs only the standard library)
and write handlers for the operations you support:

    from scripted import resource
    r = resource()

    @r.create
    def create(req):
        rec = api.create(req.input)
        return r.ok(id=rec["id"], output={"addr": rec["addr"]})

    @r.read
    def read(req):
        rec = api.get(req.id)
        if rec is None:
            return r.gone()
        return r.ok(input=req.managed(rec["spec"]), output={"addr": rec["addr"]})

    @r.update
    def update(req):
        return r.ok(output={"addr": api.update(req.id, req.input)["addr"]})

    @r.delete
    def delete(req):
        api.delete(req.id)

    r.main()

What it does for you:

- Operations without a handler, including ones added to the provider after
  this file was written, answer ``not_implemented`` so the provider's default
  applies. That is the forward-compatibility rule of the protocol, made
  structural.
- Exceptions become ``{"errors": [...]}`` with exit status 1 and a traceback
  on stderr.
- ``stdout`` is reserved for the response: while a handler runs, ``print``
  goes to stderr, and exactly one JSON document is written at the end.
- ``req`` is an object (``req.input``, ``req.id``, ``req.prior_input``, ...)
  and ``req.managed(live)`` picks out of a live object only the fields
  Terraform manages, the most common source of phantom drift.
- Response builders check that what you return makes sense for the operation
  (``r.replace()`` from ``create`` raises here instead of being ignored by the
  provider).
- ``python3 manage.py --op read --id web-1`` runs one operation from the
  command line, building the request for you, so you can try your program
  without Terraform.
- The provider starts the program once and sends one request per line;
  ``r.main()`` answers each and exits when stdin closes. Requests are handled
  at the same time, on a pool of worker threads, so handlers must keep to
  local variables; hold clients that are not thread-safe through
  ``r.local()``, which builds one per worker. ``resource(concurrent=4)`` sets
  the number of workers, ``resource(concurrent=False)`` handles one request
  at a time.

Get the copy that matches your provider with
``terraform-provider-scripted --emit-helper > scripted.py``.
"""

import argparse
import contextlib
import json
import sys
import threading
import time
import traceback
from concurrent.futures import ThreadPoolExecutor

__all__ = ["resource", "Request", "ScriptError", "PROTOCOL", "HELPER_VERSION"]

#: The protocol version this helper was written for.
PROTOCOL = 1

#: The provider version this file was emitted by ("dev" in the source tree;
#: `terraform-provider-scripted --emit-helper` fills in its own version).
HELPER_VERSION = "dev"


class ScriptError(Exception):
    """Raise from a handler to fail the operation with one or more messages."""

    def __init__(self, *messages):
        super().__init__(*messages)
        self.messages = [str(m) for m in messages] or ["operation failed"]


class Request:
    """One request from the provider, with attribute access to its fields."""

    _fields = (
        "protocol", "op", "action", "id", "input", "prior_input", "prior_output",
        "input_changed", "unknown", "private", "source", "provider_input", "context",
    )

    def __init__(self, data):
        self.raw = data
        for f in self._fields:
            setattr(self, f, data.get(f))
        if self.unknown is None:
            self.unknown = []

    def managed(self, live, default=None):
        """Return only the fields of ``live`` that Terraform manages.

        Those are the keys of the request's ``input``. A field the live object
        does not report keeps the value Terraform has (``default`` if given),
        so that write-only parameters are neither drift nor false drift.
        """
        if not isinstance(self.input, dict):
            return live
        return {k: live.get(k, self.input[k] if default is None else default) for k in self.input}

    def __repr__(self):
        return "Request(%s)" % json.dumps(self.raw)


class resource:  # noqa: N801 - reads as a declaration: r = resource()
    """Registry of handlers plus the response builders they use."""

    #: Response fields the provider acts on, per operation (plus
    #: not_implemented, warnings and errors everywhere).
    applicable = {
        "plan": {"requires_replace", "output", "private"},
        "create": {"id", "output", "private"},
        "read": {"exists", "input", "output", "id", "private"},
        "update": {"output", "private", "id"},
        "delete": set(),
        "import": {"id", "input", "output", "program", "working_dir", "environment", "context", "private"},
        "move": {"id", "input", "output", "program", "working_dir", "environment", "context", "private"},
        "data": {"output"},
    }

    #: Worker threads when ``concurrent=True``: Terraform's default
    #: parallelism. Raise it (``resource(concurrent=20)``) alongside
    #: ``terraform -parallelism=20``; the pool only grows to what is used.
    DEFAULT_WORKERS = 10

    def __init__(self, concurrent=True):
        """Handlers run on a pool of worker threads, so that ten resources
        take the time of one and a ``r.local()`` client is built once per
        worker rather than per request. Pass an int for the number of workers
        (a rate-limited API wants few), or ``False`` for a single worker,
        handling one request at a time, for handlers that share mutable state.
        Clients that are not thread-safe (``requests.Session``, database
        connections) go through ``r.local()``."""
        self._handlers = {}
        self._current = threading.local()
        if concurrent is True:
            self._workers = self.DEFAULT_WORKERS
        elif isinstance(concurrent, int) and not isinstance(concurrent, bool) and concurrent > 0:
            self._workers = concurrent
        else:
            self._workers = 1

    # -- registering handlers ---------------------------------------------

    def op(self, name):
        """Register a handler for operation ``name`` (any name, even future ones)."""

        def register(fn):
            self._handlers[name] = fn
            return fn

        return register

    def create(self, fn):
        return self.op("create")(fn)

    def read(self, fn):
        return self.op("read")(fn)

    def update(self, fn):
        return self.op("update")(fn)

    def delete(self, fn):
        return self.op("delete")(fn)

    def plan(self, fn):
        return self.op("plan")(fn)

    def import_(self, fn):
        return self.op("import")(fn)

    def move(self, fn):
        return self.op("move")(fn)

    def data(self, fn):
        return self.op("data")(fn)

    # -- per-thread state -------------------------------------------------------

    def local(self, factory):
        """One instance per worker thread, made on first use and kept for the
        life of the process: ``session = r.local(requests.Session)`` then
        ``session()`` inside handlers. The way to hold clients and connections
        that are not thread-safe; at most as many are built as there are
        workers. A process lives for one Terraform graph walk, so this is a
        cache, not storage: anything that must persist goes in ``private``."""
        store = threading.local()

        def get():
            try:
                return store.value
            except AttributeError:
                store.value = factory()
                return store.value

        return get

    # -- building responses ------------------------------------------------

    def ok(self, **fields):
        """A response with the given fields, checked against the operation."""
        op = getattr(self._current, "op", None)
        if op in self.applicable:
            extra = set(fields) - self.applicable[op] - {"warnings"}
            if extra:
                raise ScriptError("%s may not return %s" % (op, ", ".join(sorted(extra))))
        return fields

    def gone(self):
        """From ``read``: the object no longer exists."""
        if getattr(self._current, "op", None) not in (None, "read"):
            raise ScriptError("gone() is only meaningful in read")
        return {"exists": False}

    def replace(self, **fields):
        """From ``plan``: the change needs delete-and-create."""
        if getattr(self._current, "op", None) not in (None, "plan"):
            raise ScriptError("replace() is only meaningful in plan")
        return dict(self.ok(**fields), requires_replace=True)

    def not_implemented(self):
        """Let the provider apply its default for this operation."""
        return {"not_implemented": True}

    def fail(self, *messages):
        """Fail the operation with the given messages."""
        raise ScriptError(*messages)

    # -- running ------------------------------------------------------------

    def handle(self, data):
        """Run one request (a dict) and return the response (a dict)."""
        req = Request(data)
        handler = self._handlers.get(req.op)
        if handler is None:
            return self._finish(self.not_implemented(), req)
        self._current.op = req.op
        try:
            resp = handler(req)
        finally:
            self._current.op = None
        if resp is None:
            resp = {}
        if not isinstance(resp, dict):
            raise ScriptError("%s handler returned %r, not a dict" % (req.op, type(resp).__name__))
        return self._finish(resp, req)

    def _finish(self, resp, req):
        resp.setdefault("protocol", PROTOCOL)
        if req.raw.get("request_id") is not None:
            resp["request_id"] = req.raw["request_id"]
        return resp

    def main(self, argv=None):
        """Serve requests: one per line on stdin until it closes, one response
        line each, failures reported per request. With command-line flags
        instead, build one request from them, answer it pretty-printed and
        exit 1 on failure (for trying a program by hand).
        """
        argv = sys.argv[1:] if argv is None else argv
        if argv:
            resp, ok = self._run(_request_from_args(argv))
            _emit(resp, pretty=True)
            sys.exit(0 if ok else 1)
        self._serve()

    def _serve(self):
        with contextlib.redirect_stdout(sys.stderr), ThreadPoolExecutor(max_workers=self._workers) as pool:
            for line in sys.stdin:
                if line.strip():
                    pool.submit(self._answer, line)
            # Leaving the block waits for the requests still being handled.

    def _answer(self, line):
        """Answer one request line. Every line gets exactly one response, whatever
        happens: a worker thread has nobody else to report to."""
        request_id = None
        try:
            data = json.loads(line)
            if not isinstance(data, dict):
                raise ScriptError("request is not a JSON object")
            request_id = data.get("request_id")
            started = time.monotonic()
            resp, _ = self._run(data)
            resp.setdefault("duration_ms", round((time.monotonic() - started) * 1000, 1))
        except ScriptError as e:
            resp = {"errors": e.messages, "protocol": PROTOCOL}
        except Exception as e:  # noqa: BLE001 - see the docstring
            traceback.print_exc(file=sys.stderr)
            resp = {"errors": ["%s: %s" % (type(e).__name__, e)], "protocol": PROTOCOL}
        if request_id is not None:
            resp["request_id"] = request_id
        _emit(resp, pretty=False)

    def _run(self, data):
        """Handle one request; never raises. Returns (response, succeeded)."""
        try:
            return self.handle(data), True
        except ScriptError as e:
            return {"errors": e.messages, "protocol": PROTOCOL}, False
        except Exception as e:  # noqa: BLE001 - every failure must become a response
            traceback.print_exc(file=sys.stderr)
            return {"errors": ["%s: %s" % (type(e).__name__, e)], "protocol": PROTOCOL}, False


_EMIT_LOCK = threading.Lock()


def _emit(resp, pretty):
    out = sys.__stdout__  # the real stdout, even while print() is redirected
    with _EMIT_LOCK:
        out.write(json.dumps(resp, indent=2 if pretty else None, default=str))
        out.write("\n")
        out.flush()


def _request_from_args(argv):
    """Build a request from command-line flags, for trying a program by hand."""
    p = argparse.ArgumentParser(description="Run one scripted operation without Terraform.")
    p.add_argument("--op", required=True)
    p.add_argument("--action", help="plan only: create, update, delete or none")
    p.add_argument("--id")
    for name in ("input", "prior-input", "prior-output", "private", "provider-input", "context", "source"):
        p.add_argument("--" + name, metavar="JSON")
    p.add_argument("--input-changed", action="store_true")
    a = p.parse_args(argv)

    def js(v):
        return json.loads(v) if v is not None else None

    return {
        "protocol": PROTOCOL,
        "op": a.op,
        "action": a.action,
        "id": a.id,
        "input": js(a.input),
        "prior_input": js(a.prior_input),
        "prior_output": js(a.prior_output),
        "input_changed": a.input_changed,
        "unknown": [],
        "private": js(a.private),
        "provider_input": js(a.provider_input),
        "context": js(a.context),
        "source": js(a.source),
    }
