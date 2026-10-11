# ---------------------------------------------------------------------------
#  scripted.py -- helper library for programs run by terraform-provider-scripted
#
#  DO NOT EDIT. The provider writes this file itself, for its own version, to
#  .terraform/scripted/python/ (or $TF_DATA_DIR/scripted/python/), and puts
#  that directory on the PYTHONPATH of every program it runs, so programs
#  import it without keeping a copy. Point your editor there for completion
#  (VS Code: "python.analysis.extraPaths"). To run a program without
#  Terraform, or to pin a copy next to it (which then takes precedence), run:
#
#      terraform-provider-scripted --emit-python-helper > scripted.py
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

The provider puts this module on your program's PYTHONPATH (it needs only the
standard library); write handlers for the operations you support:

    from scripted import Request, Response, resource
    r = resource()

    @r.create
    def create(req: Request) -> Response:
        rec = api.create(req.input)
        return r.ok(id=rec["id"], output={"addr": rec["addr"]})

    @r.read
    def read(req: Request) -> Response:
        rec = api.get(req.id)
        if rec is None:
            return r.gone()
        return r.ok(input=req.managed(rec["spec"]), output={"addr": rec["addr"]})

    @r.update
    def update(req: Request) -> Response:
        return r.ok(output={"addr": api.update(req.id, req.input)["addr"]})

    @r.delete
    def delete(req: Request) -> None:
        api.delete(req.id)

The annotations are optional, but with them an editor completes ``req.``
fields and flags a mistyped response field such as ``r.ok(ouput=...)``.

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

Programs run by the provider import the copy matching its version. For an
editor or for running without Terraform, get it with
``terraform-provider-scripted --emit-python-helper > scripted.py``.
"""

from __future__ import annotations

import argparse
import contextlib
import json
import sys
import threading
import time
import traceback
from concurrent.futures import ThreadPoolExecutor
from typing import Any, Callable, Dict, List, Literal, NoReturn, Optional, Set, Tuple, TypeVar, Union

__all__ = [
    "HELPER_VERSION", "PROTOCOL", "Action", "Handler", "Request", "Response", "ScriptError", "resource",
]

#: The protocol version this helper was written for.
PROTOCOL = 1

#: The provider version this file was emitted by ("dev" in the source tree;
#: the provider fills in its own version when it writes or emits this file).
HELPER_VERSION = "dev"

#: What Terraform intends, in a ``plan`` request (``Request.action``).
Action = Literal["create", "update", "delete", "none"]

#: A response: the dict a handler returns, as built by ``resource.ok()`` and
#: the other builders.
Response = Dict[str, Any]

#: A handler: takes the request, returns a response (``None`` means ``{}``).
Handler = Callable[["Request"], Optional[Response]]

_H = TypeVar("_H", bound=Handler)
_T = TypeVar("_T")


class _Unset:
    """Marks a response field that was not passed, as opposed to passed null."""

    def __repr__(self) -> str:
        return "<unset>"


_UNSET: Any = _Unset()


class ScriptError(Exception):
    """Raise from a handler to fail the operation with one or more messages
    (``r.fail(...)`` does it for you)."""

    messages: List[str]

    def __init__(self, *messages: object) -> None:
        super().__init__(*messages)
        self.messages = [str(m) for m in messages] or ["operation failed"]


class Request:
    """One request from the provider, with attribute access to its fields.

    Fields a request does not carry are ``None``, except ``id`` (``""``),
    ``unknown`` (``[]``) and ``source`` (``{}``), so that handlers need no
    checks for fields their operation always has. The whole request, as
    received, is in ``raw``.
    """

    #: The protocol version the provider speaks (see ``PROTOCOL``).
    protocol: Optional[int]
    #: The operation: plan, create, read, update, delete, import, move or
    #: data, or one added in a later protocol version.
    op: str
    #: plan only: what Terraform intends.
    action: Optional[Action]
    #: The object's id; ``""`` for create, plan of a create, and data.
    id: str
    #: The desired state, the resource's ``input``; null for delete and read
    #: after an import or default move. Values not known yet are null and
    #: listed in ``unknown``.
    input: Any
    #: ``input`` as recorded in state (update, plan).
    prior_input: Any
    #: ``output`` as recorded in state.
    prior_output: Any
    #: Whether ``input`` itself changed (update and plan of an update).
    input_changed: Optional[bool]
    #: plan only: paths in ``input`` not known until apply, such as
    #: ``"name"`` or ``"tags.env"``, or ``"."`` when all of it is unknown.
    unknown: List[str]
    #: What the program last returned as ``private``.
    private: Any
    #: move only: the source resource, as {provider, type, schema_version,
    #: state}; ``{}`` otherwise.
    source: Dict[str, Any]
    #: The provider block's ``input``.
    provider_input: Any
    #: The resource's (or data source's) ``context``.
    context: Any
    #: The request as received.
    raw: Dict[str, Any]

    def __init__(self, data: Dict[str, Any]) -> None:
        self.raw = data
        self.protocol = data.get("protocol")
        self.op = data.get("op") or ""
        self.action = data.get("action")
        self.id = data.get("id") or ""
        self.input = data.get("input")
        self.prior_input = data.get("prior_input")
        self.prior_output = data.get("prior_output")
        self.input_changed = data.get("input_changed")
        self.unknown = data.get("unknown") or []
        self.private = data.get("private")
        self.source = data.get("source") or {}
        self.provider_input = data.get("provider_input")
        self.context = data.get("context")

    def managed(self, live: Dict[str, Any], default: Any = None) -> Any:
        """Return only the fields of ``live`` that Terraform manages.

        Those are the keys of the request's ``input``. A field the live object
        does not report keeps the value Terraform has (``default`` if given),
        so that write-only parameters are neither drift nor false drift.
        """
        if not isinstance(self.input, dict):
            return live
        return {k: live.get(k, self.input[k] if default is None else default) for k in self.input}

    def __repr__(self) -> str:
        return "Request(%s)" % json.dumps(self.raw)


class resource:  # noqa: N801 - reads as a declaration: r = resource()
    """Registry of handlers plus the response builders they use."""

    #: Response fields the provider acts on, per operation (plus
    #: not_implemented, warnings and errors everywhere).
    applicable: Dict[str, Set[str]] = {
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

    def __init__(self, concurrent: Union[bool, int] = True) -> None:
        """Handlers run on a pool of worker threads, so that ten resources
        take the time of one and a ``r.local()`` client is built once per
        worker rather than per request. Pass an int for the number of workers
        (a rate-limited API wants few), or ``False`` for a single worker,
        handling one request at a time, for handlers that share mutable state.
        Clients that are not thread-safe (``requests.Session``, database
        connections) go through ``r.local()``."""
        self._handlers: Dict[str, Handler] = {}
        self._current = threading.local()
        self._workers: int
        if concurrent is True:
            self._workers = self.DEFAULT_WORKERS
        elif isinstance(concurrent, int) and not isinstance(concurrent, bool) and concurrent > 0:
            self._workers = concurrent
        else:
            self._workers = 1

    # -- registering handlers ---------------------------------------------

    def op(self, name: str) -> Callable[[_H], _H]:
        """Register a handler for operation ``name`` (any name, even future ones)."""

        def register(fn: _H) -> _H:
            self._handlers[name] = fn
            return fn

        return register

    def create(self, fn: _H) -> _H:
        """Handle ``create``: make the object from ``req.input`` and return
        ``r.ok(id=..., output=...)``. The id is required; returning the id of
        an object that already exists adopts it."""
        return self.op("create")(fn)

    def read(self, fn: _H) -> _H:
        """Handle ``read``, on every refresh: return ``r.ok(input=...,
        output=...)`` with the object as it is (``req.managed(live)`` for the
        input), or ``r.gone()`` if it no longer exists. Leaving ``input`` out
        keeps what state has."""
        return self.op("read")(fn)

    def update(self, fn: _H) -> _H:
        """Handle ``update``, when ``input`` changed: bring the object to
        ``req.input`` (``req.prior_input`` is what state had) and return
        ``r.ok(output=...)``."""
        return self.op("update")(fn)

    def delete(self, fn: _H) -> _H:
        """Handle ``delete``: remove the object ``req.id``. Return nothing; an
        object that is already gone is not an error."""
        return self.op("delete")(fn)

    def plan(self, fn: _H) -> _H:
        """Handle ``plan``, on every plan: ``req.action`` says what Terraform
        intends. Return ``r.fail(...)`` to reject the configuration,
        ``r.replace()`` to make an update delete-and-create, ``r.ok(output=...)``
        to promise output values (the apply must then return them), or
        ``r.not_implemented()`` for the provider's default."""
        return self.op("plan")(fn)

    def import_(self, fn: _H) -> _H:
        """Handle ``import``: ``req.id`` is what the user gave. Return
        ``r.ok(id=..., input=..., output=...)``; without ``input`` the first
        plan records the configuration's."""
        return self.op("import")(fn)

    def move(self, fn: _H) -> _H:
        """Handle ``move``, for a ``moved`` block from another resource type:
        ``req.source`` holds the source's provider, type and raw state. Return
        ``r.ok(id=..., input=..., output=...)``, or ``r.not_implemented()``
        for the default (keep the id, read the rest)."""
        return self.op("move")(fn)

    def data(self, fn: _H) -> _H:
        """Handle the ``scripted_data`` data source: look up ``req.input`` and
        return ``r.ok(output=...)``."""
        return self.op("data")(fn)

    # -- per-thread state -------------------------------------------------------

    def local(self, factory: Callable[[], _T]) -> Callable[[], _T]:
        """One instance per worker thread, made on first use and kept for the
        life of the process: ``session = r.local(requests.Session)`` then
        ``session()`` inside handlers. The way to hold clients and connections
        that are not thread-safe; at most as many are built as there are
        workers. A process lives for one Terraform graph walk, so this is a
        cache, not storage: anything that must persist goes in ``private``."""
        store = threading.local()

        def get() -> _T:
            try:
                value: _T = store.value
            except AttributeError:
                value = factory()
                store.value = value
            return value

        return get

    # -- building responses ------------------------------------------------

    def ok(
        self,
        *,
        id: str = _UNSET,  # noqa: A002 - the protocol's field name
        input: Any = _UNSET,  # noqa: A002 - the protocol's field name
        output: Any = _UNSET,
        private: Any = _UNSET,
        exists: bool = _UNSET,
        requires_replace: bool = _UNSET,
        warnings: List[str] = _UNSET,
        program: List[str] = _UNSET,
        working_dir: str = _UNSET,
        environment: Dict[str, str] = _UNSET,
        context: Any = _UNSET,
    ) -> Response:
        """A response with the given fields, checked against the operation.

        Only the fields passed are in the response; which ones an operation
        may return is in ``applicable``, and returning another raises here
        rather than being ignored by the provider. ``warnings`` goes with any.
        """
        fields = {k: v for k, v in (
            ("id", id), ("input", input), ("output", output), ("private", private), ("exists", exists),
            ("requires_replace", requires_replace), ("warnings", warnings), ("program", program),
            ("working_dir", working_dir), ("environment", environment), ("context", context),
        ) if v is not _UNSET}
        op = getattr(self._current, "op", None)
        if op in self.applicable:
            extra = set(fields) - self.applicable[op] - {"warnings"}
            if extra:
                raise ScriptError("%s may not return %s" % (op, ", ".join(sorted(extra))))
        return fields

    def gone(self) -> Response:
        """From ``read``: the object no longer exists."""
        if getattr(self._current, "op", None) not in (None, "read"):
            raise ScriptError("gone() is only meaningful in read")
        return {"exists": False}

    def replace(self, *, output: Any = _UNSET, private: Any = _UNSET, warnings: List[str] = _UNSET) -> Response:
        """From ``plan``: the change needs delete-and-create (optionally with
        the output the create will return, private state and warnings)."""
        if getattr(self._current, "op", None) not in (None, "plan"):
            raise ScriptError("replace() is only meaningful in plan")
        return dict(self.ok(output=output, private=private, warnings=warnings), requires_replace=True)

    def not_implemented(self) -> Response:
        """Let the provider apply its default for this operation."""
        return {"not_implemented": True}

    def fail(self, *messages: object) -> NoReturn:
        """Fail the operation with the given messages."""
        raise ScriptError(*messages)

    # -- running ------------------------------------------------------------

    def handle(self, data: Dict[str, Any]) -> Response:
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

    def _finish(self, resp: Response, req: Request) -> Response:
        resp.setdefault("protocol", PROTOCOL)
        if req.raw.get("request_id") is not None:
            resp["request_id"] = req.raw["request_id"]
        return resp

    def main(self, argv: Optional[List[str]] = None) -> None:
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

    def _serve(self) -> None:
        with contextlib.redirect_stdout(sys.stderr), ThreadPoolExecutor(max_workers=self._workers) as pool:
            for line in sys.stdin:
                if line.strip():
                    pool.submit(self._answer, line)
            # Leaving the block waits for the requests still being handled.

    def _answer(self, line: str) -> None:
        """Answer one request line. Every line gets exactly one response, whatever
        happens: a worker thread has nobody else to report to."""
        request_id = None
        resp: Response
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

    def _run(self, data: Dict[str, Any]) -> Tuple[Response, bool]:
        """Handle one request; never raises. Returns (response, succeeded)."""
        try:
            return self.handle(data), True
        except ScriptError as e:
            return {"errors": e.messages, "protocol": PROTOCOL}, False
        except Exception as e:  # noqa: BLE001 - every failure must become a response
            traceback.print_exc(file=sys.stderr)
            return {"errors": ["%s: %s" % (type(e).__name__, e)], "protocol": PROTOCOL}, False


_EMIT_LOCK = threading.Lock()


def _emit(resp: Response, pretty: bool) -> None:
    # The real stdout, even while print() is redirected to stderr.
    out = sys.__stdout__ if sys.__stdout__ is not None else sys.stdout
    with _EMIT_LOCK:
        out.write(json.dumps(resp, indent=2 if pretty else None, default=str))
        out.write("\n")
        out.flush()


def _request_from_args(argv: List[str]) -> Dict[str, Any]:
    """Build a request from command-line flags, for trying a program by hand."""
    p = argparse.ArgumentParser(description="Run one scripted operation without Terraform.")
    p.add_argument("--op", required=True)
    p.add_argument("--action", help="plan only: create, update, delete or none")
    p.add_argument("--id")
    for name in ("input", "prior-input", "prior-output", "private", "provider-input", "context", "source"):
        p.add_argument("--" + name, metavar="JSON")
    p.add_argument("--input-changed", action="store_true")
    a = p.parse_args(argv)

    def js(v: Optional[str]) -> Any:
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
