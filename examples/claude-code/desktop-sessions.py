#!/usr/bin/env python3
"""Discover host-provided existing sessions without changing project owners."""
import importlib.util
import json
import os
from pathlib import Path
import stat
import uuid

_spec = importlib.util.spec_from_file_location('session_route_discovery', Path(__file__).with_name('session-route.py'))
route = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(route)
DEFAULT_STATE = Path.home() / '.config/tincan/desktop-wake'


def _sessions_directory(state):
    # Validate the existing private parent before creating anything beneath it.
    route._registry_directory(state / 'placeholder.json')
    directory = state / 'sessions'
    directory.mkdir(mode=0o700, exist_ok=True)
    route._registry_directory(directory / 'placeholder.json')
    return directory


def _record(directory_fd, sid, missing_ok=False):
    name = sid + '.session.json'
    try:
        fd = route._open_private_regular(directory_fd, name, os.O_RDONLY)
    except route.RouteError:
        try:
            os.stat(name, dir_fd=directory_fd, follow_symlinks=False)
        except FileNotFoundError:
            if missing_ok:
                return None
        raise
    with os.fdopen(fd) as stream:
        raw = stream.read(16385)
    if len(raw) > 16384:
        raise route.RouteError('session_record_too_large')
    value = json.loads(raw, object_pairs_hook=route._duplicate_rejecting_object)
    if (not isinstance(value, dict) or set(value) != {'version', 'session_id', 'project', 'transcript_path'}
            or type(value['version']) is not int or value['version'] != 1
            or value['session_id'] != sid or str(uuid.UUID(sid)) != sid
            or not isinstance(value['project'], str) or not Path(value['project']).is_absolute()
            or os.path.normpath(value['project']) != value['project']
            or not isinstance(value['transcript_path'], str)
            or not Path(value['transcript_path']).is_absolute()
            or Path(value['transcript_path']).name != sid + '.jsonl'):
        raise route.RouteError('invalid_session_record')
    return value


def session_deleted(sid, state=DEFAULT_STATE):
    """Return true only for Claude's explicit, private delete marker."""
    directory = _sessions_directory(state)
    with route._open_registry_directory(directory / 'records') as (fd, _):
        value = _record(fd, sid, missing_ok=True)
    if value is None:
        return False
    transcript = Path(value['transcript_path'])
    marker = transcript.with_name(sid + '.desktop-released.json')
    try:
        fd = os.open(marker, os.O_RDONLY | os.O_NOFOLLOW)
    except FileNotFoundError:
        return False
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or info.st_mode & 0o077 or info.st_size > 4096):
            raise route.RouteError('invalid_desktop_release_marker')
        with os.fdopen(fd) as stream:
            fd = -1
            released = json.load(stream, object_pairs_hook=route._duplicate_rejecting_object)
    finally:
        if fd >= 0:
            os.close(fd)
    if (not isinstance(released, dict)
            or set(released) != {'v', 'releasedAt', 'reason'}
            or released['v'] != 1
            or not isinstance(released['releasedAt'], str)
            or not isinstance(released['reason'], str)):
        raise route.RouteError('invalid_desktop_release_marker')
    return released['reason'] == 'delete'


def project_for(sid, registry=route.DEFAULT_REGISTRY, state=DEFAULT_STATE):
    if route._session_uuid(sid) != sid:
        raise route.RouteError('invalid_session_uuid')
    with route._open_registry_directory(registry) as (fd, name):
        owners = route._registry_state(fd, name, missing_ok=True) or {'routes': []}
    bound = next((row for row in owners['routes'] if row['session_id'] == sid), None)
    with route._open_registry_directory(_sessions_directory(state) / 'records') as (fd, _):
        known = _record(fd, sid, missing_ok=True)
    if bound and known and bound['project'] != known['project']:
        raise route.RouteError('session_project_conflict')
    if bound or known:
        return route._project_directory(Path((bound or known)['project']))
    raise route.RouteError('session_not_discovered')


def register(item, registry=route.DEFAULT_REGISTRY, state=DEFAULT_STATE):
    if item.get('agent_id') or item.get('agent_type'):
        raise route.RouteError('subagent_not_owner')
    sid = item['session_id']
    if route._session_uuid(sid) != sid:
        raise route.RouteError('invalid_session_uuid')
    transcript = Path(item['transcript_path'])
    if not transcript.is_absolute() or transcript.name != sid + '.jsonl':
        raise route.RouteError('wrong_transcript')
    cwd = route._project_directory(Path(item['cwd']))
    with route._open_registry_directory(registry) as (fd, name):
        owners = route._registry_state(fd, name, missing_ok=True) or {'routes': []}
    bound = next((row for row in owners['routes'] if row['session_id'] == sid), None)
    with route._open_registry_directory(_sessions_directory(state) / 'records') as (fd, _):
        with route._registry_lock(fd, 'discovery'):
            existing = _record(fd, sid, missing_ok=True)
            if bound and existing and bound['project'] != existing['project']:
                raise route.RouteError('session_project_conflict')
            project = route._project_directory(Path((bound or existing)['project'])) if bound or existing else cwd
            # CwdChanged is navigation inside the same host conversation, not
            # authority to reassign that conversation's communication owner.
            value = {'version': 1, 'session_id': sid, 'project': str(project), 'transcript_path': str(transcript)}
            if existing != value:
                route._atomic_write(fd, sid + '.session.json', value)
    return project


def candidates(project, state=DEFAULT_STATE):
    project = str(route._project_directory(project))
    directory = _sessions_directory(state)
    with route._open_registry_directory(directory / 'records') as (fd, _):
        with route._registry_lock(fd, 'discovery'):
            found = []
            for name in os.listdir(fd):
                if not name.endswith('.session.json'):
                    continue
                value = _record(fd, name[:-len('.session.json')])
                if value['project'] == project and not session_deleted(value['session_id'], state):
                    found.append(value)
    return sorted(found, key=lambda row: row['session_id'])


def known_sessions(state=DEFAULT_STATE):
    """Return validated local native records for lifecycle catalog sync."""
    directory = _sessions_directory(state)
    with route._open_registry_directory(directory / 'records') as (fd, _):
        with route._registry_lock(fd, 'discovery'):
            found = []
            for name in os.listdir(fd):
                if name.endswith('.session.json'):
                    found.append(_record(fd, name[:-len('.session.json')]))
    return sorted(found, key=lambda row: row['session_id'])
