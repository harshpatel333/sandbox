"""List, select and destroy workflow-managed devboxes by label identity, never by name prefix."""
import argparse
import datetime
import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass, field

import devbox_contract as contract

ACTIONS = ('list', 'destroy-named', 'destroy-mine', 'destroy-all', 'auto-delete')
PROJECT_IN_LINK = re.compile(r'/projects/([^/]+)/')
CLOUD_ERRORS = (RuntimeError, subprocess.TimeoutExpired, ValueError)


@dataclass
class Resource:
    kind: str
    name: str
    zone: str
    labels: dict
    created: str = ''
    link: str = ''
    users: list = field(default_factory=list)
    manifest: dict | None = None
    status: str = ''
    adopted: bool = False


@dataclass
class Box:
    name: str
    devbox_id: str
    resources: list

    def of(self, kind):
        return [r for r in self.resources if r.kind == kind]

    @property
    def instance(self):
        return next(iter(self.of('instance')), None)

    @property
    def primary(self):
        return self.instance or next(r for r in self.resources if not r.adopted)

    @property
    def zone(self):
        return next((r.zone for r in self.resources if r.zone), '')

    @property
    def owner(self):
        return self.primary.labels.get('owner', '')

    @property
    def expires_at(self):
        value = self.primary.labels.get('expires-at', '')
        return int(value) if value.isdigit() else None

    @property
    def manifest(self):
        return self.instance.manifest if self.instance else None

    @property
    def missing(self):
        return sorted(f'{kind} {name}' for kind, name in expected(self.name, self.devbox_id)
                      - {(r.kind, r.name) for r in self.resources})

    @property
    def unhandled(self):
        known = expected(self.name, self.devbox_id)
        return [f'{r.get("type")} {r.get("name")}' for r in (self.manifest or {}).get('resources', [])
                if isinstance(r, dict) and (r.get('type'), r.get('name')) not in known]

    @property
    def state(self):
        if not self.instance:
            return 'orphaned (no VM)'
        manifest_state = (self.manifest or {}).get('state')
        return f'{self.instance.status} / {manifest_state}' if manifest_state else self.instance.status

    def target(self):
        return {'name': self.name, 'zone': self.zone, 'devbox_id': self.devbox_id}


class Gcloud:
    def __init__(self, project):
        self.project = project

    def _run(self, *args, check=True):
        result = subprocess.run(['gcloud', *args, f'--project={self.project}'],
                                capture_output=True, text=True, timeout=900)
        if check and result.returncode:
            raise RuntimeError(f'gcloud {" ".join(args[:3])} failed: {result.stderr.strip()}')
        return result

    def _list(self, *args):
        return json.loads(self._run(*args, '--format=json').stdout or '[]')

    def instances(self):
        return self._list('compute', 'instances', 'list')

    def disks(self):
        return self._list('compute', 'disks', 'list')

    def buckets(self):
        return self._list('storage', 'buckets', 'list')

    def set_metadata(self, name, zone, key, value):
        with tempfile.NamedTemporaryFile('w', suffix='.json') as handle:
            handle.write(value)
            handle.flush()
            return self._failure('compute', 'instances', 'add-metadata', name, f'--zone={zone}',
                                 f'--metadata-from-file={key}={handle.name}')

    def delete_instance(self, name, zone):
        return self._failure('compute', 'instances', 'delete', name, f'--zone={zone}', '--keep-disks=all', '--quiet')

    def delete_disk(self, name, zone):
        return self._failure('compute', 'disks', 'delete', name, f'--zone={zone}', '--quiet')

    def delete_bucket(self, name):
        emptied = self._run('storage', 'rm', '--recursive', '--all-versions', f'gs://{name}/**', check=False)
        failure = self._failure('storage', 'buckets', 'delete', f'gs://{name}', '--quiet')
        return failure and '\n'.join(filter(None, [emptied.stderr.strip() if emptied.returncode else '', failure]))

    def _failure(self, *args):
        result = self._run(*args, check=False)
        return (result.stderr.strip() or f'exit status {result.returncode}') if result.returncode else None


def expected(name, devbox_id):
    return {(r['type'], r['name']) for r in contract.resources(name, devbox_id)}


def _last(url):
    return (url or '').rsplit('/', 1)[-1]


def _manifest(instance):
    for item in (instance.get('metadata') or {}).get('items') or []:
        if item.get('key') == contract.MANIFEST_METADATA_KEY:
            try:
                return contract.validate_manifest(json.loads(item.get('value') or ''))
            except Exception:
                return None
    return None


def collect(cloud):
    resources = [Resource('instance', i.get('name', ''), _last(i.get('zone')), i.get('labels') or {},
                          i.get('creationTimestamp', ''), i.get('selfLink', ''),
                          manifest=_manifest(i), status=i.get('status', ''))
                 for i in cloud.instances()]
    resources += [Resource('disk', d.get('name', ''), _last(d.get('zone')), d.get('labels') or {},
                           d.get('creationTimestamp', ''), d.get('selfLink', ''), users=d.get('users') or [])
                  for d in cloud.disks()]
    resources += [Resource('bucket', b.get('name', ''), '', b.get('labels') or {}, b.get('creation_time', ''))
                  for b in cloud.buckets()]
    return resources


def _placement(resource, project):
    if resource.kind == 'bucket':
        return None
    match = PROJECT_IN_LINK.search(resource.link)
    if not match or match.group(1) != project:
        return f'not in project {project}'
    if resource.zone != contract.ZONE:
        return f'not in zone {contract.ZONE}'
    return None


def ineligibility(resource, project):
    name = resource.labels.get('devbox-name', '')
    try:
        contract.validate_name(name)
        devbox_id = contract.check_labels(resource.labels, name)
    except contract.ContractError as error:
        return str(error)
    if (resource.kind, resource.name) not in expected(name, devbox_id):
        return f'name does not match its devbox labels ({name}, {devbox_id})'
    return _placement(resource, project)


def _unlabelled(resource, project):
    return (resource.kind != 'instance' and 'managed-by' not in resource.labels
            and not set(contract.FORBIDDEN_LABELS) & set(resource.labels) and _placement(resource, project) is None)


def inventory(resources, project):
    """Group labelled resources by identity, then adopt the unlabelled boot disk or bucket that create
    can leave between its create and label calls, only by exact name under exactly one labelled box."""
    groups, ignored = {}, []
    for resource in resources:
        reason = ineligibility(resource, project)
        if reason:
            if resource.labels.get('managed-by') == contract.MANAGED_BY:
                ignored.append((resource, reason))
            continue
        key = (resource.labels['devbox-name'], resource.labels['devbox-id'])
        groups.setdefault(key, []).append(resource)
    claims = {}
    for key in groups:
        for item in expected(*key):
            claims.setdefault(item, []).append(key)
    for resource in resources:
        owners = claims.get((resource.kind, resource.name), [])
        if not owners or not _unlabelled(resource, project):
            continue
        if len(owners) > 1:
            ignored.append((resource, f'unlabelled and claimed by {len(owners)} devboxes'))
            continue
        resource.adopted = True
        groups[owners[0]].append(resource)
    boxes = [Box(name, devbox_id, sorted(group, key=lambda r: (r.kind, r.name)))
             for (name, devbox_id), group in sorted(groups.items())]
    return boxes, ignored


def select(boxes, action, *, name='', owner='', now=0):
    if action == 'destroy-named':
        return [b for b in boxes if b.name == name]
    if action == 'destroy-mine':
        return [b for b in boxes if b.owner == owner]
    if action == 'destroy-all':
        return list(boxes)
    if action == 'auto-delete':
        return [b for b in boxes if b.expires_at is not None and b.expires_at <= now]
    return []


def set_token(targets):
    canonical = json.dumps(sorted((t['name'], t['zone'], t['devbox_id']) for t in targets))
    return hashlib.sha256(canonical.encode()).hexdigest()[:16]


def _utc(epoch):
    return datetime.datetime.fromtimestamp(epoch, datetime.UTC).strftime('%Y-%m-%d %H:%M UTC')


def _age(box, now):
    stamp = (box.manifest or {}).get('created_at') or box.primary.created
    try:
        created = datetime.datetime.fromisoformat(stamp.replace('Z', '+00:00')).timestamp()
    except (ValueError, AttributeError):
        return '?'
    hours = int((now - created) // 3600)
    return f'{hours // 24}d {hours % 24}h' if hours >= 24 else f'{hours}h'


def _cell(value):
    return str(value).replace('|', '\\|')


def _revisions(box):
    manifest = box.manifest or {}
    running = next((v for v in (manifest.get('components'), manifest.get('revisions')) if isinstance(v, dict) and v), {})
    groups = {}
    for component, revision in running.items():
        if isinstance(revision, dict):
            key = f'{revision.get("branch") or "?"} @ {str(revision.get("sha") or "?")[:12]}'
            groups.setdefault(key, []).append(str(component))
    return _cell('; '.join(f'{key} ({", ".join(parts)})' for key, parts in groups.items()) or '?')


def render(boxes, ignored, now):
    if not boxes:
        lines = ['No workflow-managed devboxes found.']
    else:
        lines = ['| Name | Owner | Zone | State | Age | Expires at | Revisions | devbox-id |',
                 '| -- | -- | -- | -- | -- | -- | -- | -- |']
        for box in boxes:
            notes = [f'missing {", ".join(box.missing)}'] if box.missing else []
            notes += [f'unlabelled {", ".join(f"{r.kind} {r.name}" for r in box.resources if r.adopted)}'] \
                if any(r.adopted for r in box.resources) else []
            state = box.state + (f' (partial: {"; ".join(notes)})' if notes else '')
            expires = _utc(box.expires_at) if box.expires_at is not None else 'unset (never auto-deleted)'
            lines.append(f'| `{box.name}` | {box.owner or "?"} | {box.zone or "-"} | {state} | {_age(box, now)} '
                         f'| {expires} | {_revisions(box)} | `{box.devbox_id}` |')
        reachable = [b for b in boxes if b.instance]
        if reachable:
            lines += ['', 'Access (IAP tunnel):', '']
            for box in reachable:
                lines += [f'`{box.name}`:', '```', contract.forwarding_command(box.name, box.zone), '```']
    if ignored:
        lines += ['', 'Ignored resources (never destroyed by these workflows):', '']
        lines += [f'- {r.kind} `{r.name}`: {reason}' for r, reason in ignored]
    return '\n'.join(lines)


def render_targets(targets):
    lines = ['| Name | Zone | devbox-id |', '| -- | -- | -- |']
    return '\n'.join(lines + [f'| `{t["name"]}` | {t["zone"] or "-"} | `{t["devbox_id"]}` |' for t in targets])


def plan(cloud, project, action, *, name='', actor='', confirm='', now=0):
    """Return (targets, report); raise SystemExit when the request must not proceed."""
    if action not in ACTIONS:
        raise SystemExit(f'Unknown action {action!r}')
    if action == 'destroy-named':
        try:
            contract.validate_name(name)
        except contract.ContractError as error:
            raise SystemExit(str(error)) from None
    boxes, ignored = inventory(collect(cloud), project)
    report = [render(boxes, ignored, now)]
    if action == 'list':
        return [], '\n'.join(report)
    owner = contract.normalize_owner(actor) if action == 'destroy-mine' else ''
    targets = [b.target() for b in select(boxes, action, name=name, owner=owner, now=now)]
    if not targets:
        return [], '\n'.join(report + ['', f'Nothing to destroy for {action}.'])
    report += ['', f'Target set for {action} ({len(targets)}):', '', render_targets(targets)]
    if action == 'destroy-all':
        token = set_token(targets)
        if not confirm:
            report += ['', f'Nothing was destroyed. To destroy exactly this set, rerun destroy-all with confirm `{token}`.']
            return [], '\n'.join(report)
        if confirm.strip() != token:
            raise SystemExit('\n'.join(report + ['', f'Confirmation does not match the current set (now `{token}`). '
                                                 'The set changed since it was approved; nothing was destroyed.']))
    return targets, '\n'.join(report)


def by_name(targets):
    names = {}
    for target in targets:
        names.setdefault(target['name'], []).append(target['devbox_id'])
    return [{'name': name, 'devbox_ids': ','.join(ids)} for name, ids in sorted(names.items())]


def find(cloud, project, name, devbox_id):
    boxes, _ = inventory(collect(cloud), project)
    return next((b for b in boxes if b.name == name and b.devbox_id == devbox_id), None)


def _delete(cloud, resource, instance):
    if resource.kind == 'bucket':
        return cloud.delete_bucket(resource.name)
    foreign = [u for u in resource.users if not instance or u != instance.link]
    if foreign:
        return f'disk {resource.name} is attached to {", ".join(map(_last, foreign))}; not deleted'
    return cloud.delete_disk(resource.name, resource.zone)


def destroy(cloud, project, name, devbox_id, *, require_expired_at=None):
    """Delete exactly the resources of this devbox identity; succeed only when none remain.

    Unlabelled resources go first and the labelled ones are kept if any of them fails,
    because the labelled resources are what lets a retry find the unlabelled ones again."""
    box = find(cloud, project, name, devbox_id)
    if box is None:
        return f'`{name}` ({devbox_id}) is already gone.'
    if require_expired_at is not None and (box.expires_at is None or box.expires_at > require_expired_at):
        return f'`{name}` ({devbox_id}) is no longer expired; left in place.'
    if box.unhandled:
        raise SystemExit(f'Refusing to destroy {name} ({devbox_id}): its manifest records resources destroy cannot '
                         f'remove ({", ".join(box.unhandled)}); nothing was deleted.')
    instance = box.instance
    if instance and box.manifest is not None:
        try:
            error = cloud.set_metadata(instance.name, instance.zone, contract.MANIFEST_METADATA_KEY,
                                       json.dumps(contract.with_state(box.manifest, 'destroying')))
        except contract.ContractError as invalid:
            error = str(invalid)
        if error:
            print(f'::warning::Could not mark {name} as destroying: {error}')
    failures = [cloud.delete_instance(instance.name, instance.zone)] if instance else []
    adopted = [_delete(cloud, r, instance) for r in box.resources if r.adopted]
    failures += adopted
    if not any(adopted):
        failures += [_delete(cloud, r, instance) for r in box.resources if r.kind != 'instance' and not r.adopted]
    try:
        remaining = find(cloud, project, name, devbox_id)
    except CLOUD_ERRORS as error:
        raise SystemExit(f'Could not verify destroy of {name} ({devbox_id}): {error}\n'
                         + '\n'.join(f'- {f}' for f in failures if f)) from None
    if remaining:
        details = [f'- {r.kind} {r.name}' for r in remaining.resources] + [f'- {f}' for f in failures if f]
        raise SystemExit(f'Destroy of {name} ({devbox_id}) incomplete; rerun to retry. Remaining:\n' + '\n'.join(details))
    return f'Destroyed `{name}` ({devbox_id}): ' + ', '.join(f'{r.kind} {r.name}' for r in box.resources)


def destroy_name(cloud, project, name, devbox_ids, *, require_expired_at=None):
    """Destroy every identity approved for one name; report each and fail if any is incomplete."""
    try:
        contract.validate_name(name)
        ids = [contract.validate_devbox_id(i) for i in devbox_ids.split(',')]
    except contract.ContractError as error:
        raise SystemExit(str(error)) from None
    reports, incomplete = [], []
    for devbox_id in ids:
        try:
            reports.append(destroy(cloud, project, name, devbox_id, require_expired_at=require_expired_at))
        except SystemExit as stop:
            reports.append(str(stop.code))
            incomplete.append(devbox_id)
        except CLOUD_ERRORS as error:
            reports.append(f'Destroy of {name} ({devbox_id}) failed: {error}; rerun to retry.')
            incomplete.append(devbox_id)
    if incomplete:
        raise SystemExit('\n\n'.join(reports + [f'Destroy incomplete for {name}: {", ".join(incomplete)}']))
    return '\n\n'.join(reports)


def _publish(report, outputs=None):
    print(report)
    if os.environ.get('GITHUB_STEP_SUMMARY'):
        with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as handle:
            handle.write(report + '\n')
    if outputs and os.environ.get('GITHUB_OUTPUT'):
        with open(os.environ['GITHUB_OUTPUT'], 'a') as handle:
            handle.writelines(f'{key}={value}\n' for key, value in outputs.items())


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    planning = commands.add_parser('plan')
    planning.add_argument('--action', required=True, choices=ACTIONS)
    planning.add_argument('--name', default='')
    planning.add_argument('--actor', default='')
    planning.add_argument('--confirm', default='')
    destroying = commands.add_parser('destroy')
    destroying.add_argument('--name', required=True)
    destroying.add_argument('--devbox-ids', required=True, help='comma-separated devbox-ids for this name')
    destroying.add_argument('--require-expired', action='store_true')
    args = parser.parse_args(argv)
    cloud, now = Gcloud(contract.PROJECT), int(time.time())
    try:
        if args.command == 'plan':
            targets, report = plan(cloud, contract.PROJECT, args.action, name=args.name, actor=args.actor,
                                   confirm=args.confirm, now=now)
            matrix = by_name(targets)
            _publish(report, {'targets': json.dumps(matrix), 'count': len(matrix)})
        else:
            _publish(destroy_name(cloud, contract.PROJECT, args.name, args.devbox_ids,
                                  require_expired_at=now if args.require_expired else None))
    except SystemExit as stop:
        if isinstance(stop.code, str):
            _publish(stop.code)
            raise SystemExit(1) from None
        raise
    except CLOUD_ERRORS as error:
        _publish(f'Failed: {error}. Rerun to retry.')
        raise SystemExit(1) from None


if __name__ == '__main__':
    sys.exit(main())
