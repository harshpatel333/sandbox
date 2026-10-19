"""Devbox destroy must select by label identity in the dev project and never broaden its approved set."""
from datetime import datetime, timezone
import json
import unittest

import devbox_contract as contract
import devbox_inventory as inv

PROJECT = contract.PROJECT
ZONE = contract.ZONE
NOW = 1_800_000_000


def link(kind, name, project=PROJECT, zone=ZONE):
    return f'https://www.googleapis.com/compute/v1/projects/{project}/zones/{zone}/{kind}/{name}'


def at(epoch):
    return datetime.fromtimestamp(epoch, timezone.utc)


def labels(name, devbox_id, owner='alice', expires_at=NOW + 3600, **extra):
    return {**contract.labels(name, devbox_id, owner, at(expires_at)), **extra}


class FakeCloud:
    def __init__(self):
        self.instances_, self.disks_, self.buckets_ = [], [], []
        self.calls, self.fail = [], set()

    def add_box(self, name, devbox_id, *, vm=True, disks=True, bucket=True, project=PROJECT, **label_args):
        tags = labels(name, devbox_id, **label_args)
        manifest = contract.with_state(contract.new_manifest(
            name=name, devbox_id=devbox_id, owner=tags['owner'], actor=tags['owner'],
            created_at=at(NOW - 26 * 3600), expires_at=at(int(tags['expires-at'])),
            revisions={'sandbox': {'branch': 'main', 'sha': 'a' * 40},
                       'ui': {'branch': 'feature-x', 'sha': 'b' * 40}}), 'ready')
        if vm:
            self.instances_.append({'name': name, 'zone': f'projects/{project}/zones/{ZONE}', 'status': 'RUNNING',
                                    'labels': dict(tags), 'selfLink': link('instances', name, project),
                                    'creationTimestamp': '2027-01-15T06:00:00.000-00:00',
                                    'metadata': {'items': [{'key': contract.MANIFEST_METADATA_KEY,
                                                            'value': json.dumps(manifest)}]}})
        if disks:
            for disk in (name, name + '-data'):
                self.disks_.append({'name': disk, 'zone': f'projects/{project}/zones/{ZONE}', 'labels': dict(tags),
                                    'selfLink': link('disks', disk, project),
                                    'users': [link('instances', name, project)] if vm else []})
        if bucket:
            self.buckets_.append({'name': contract.bucket_name(devbox_id), 'labels': dict(tags)})

    def instances(self):
        return [dict(i) for i in self.instances_]

    def disks(self):
        return [dict(d) for d in self.disks_]

    def buckets(self):
        return [dict(b) for b in self.buckets_]

    def _delete(self, kind, collection, name):
        self.calls.append((kind, name))
        if (kind, name) in self.fail:
            return f'{kind} {name}: permission denied'
        collection[:] = [item for item in collection if item['name'] != name]
        return None

    def set_metadata(self, name, zone, key, value):
        self.calls.append(('metadata', name, json.loads(value)['state']))

    def delete_instance(self, name, zone):
        for disk in self.disks_:
            disk['users'] = [u for u in disk['users'] if u != link('instances', name)]
        return self._delete('instance', self.instances_, name)

    def delete_disk(self, name, zone):
        return self._delete('disk', self.disks_, name)

    def delete_bucket(self, name):
        return self._delete('bucket', self.buckets_, name)


def boxes(cloud):
    return inv.inventory(inv.collect(cloud), PROJECT)


class EligibilityTest(unittest.TestCase):
    def test_complete_box_groups_all_four_resources(self):
        cloud = FakeCloud()
        cloud.add_box('alice-dev-a7f3c9', '3f9c0a1b2c4d')
        [box], ignored = boxes(cloud)
        self.assertEqual(ignored, [])
        self.assertEqual(sorted((r.kind, r.name) for r in box.resources), [
            ('bucket', 'superserve-devbox-3f9c0a1b2c4d'), ('disk', 'alice-dev-a7f3c9'),
            ('disk', 'alice-dev-a7f3c9-data'), ('instance', 'alice-dev-a7f3c9')])
        self.assertEqual((box.zone, box.owner, box.missing), (ZONE, 'alice', []))
        self.assertEqual(box.state, 'RUNNING / ready')

    def test_prefix_lookalikes_and_unlabelled_resources_are_never_selected(self):
        cloud = FakeCloud()
        cloud.instances_.append({'name': 'alice-dev-a7f3c9', 'zone': ZONE, 'labels': {},
                                 'selfLink': link('instances', 'alice-dev-a7f3c9')})
        cloud.disks_.append({'name': 'alice-dev-a7f3c9-data', 'zone': ZONE,
                             'labels': {'devbox-name': 'alice-dev-a7f3c9', 'devbox-id': '3f9c0a1b2c4d'},
                             'selfLink': link('disks', 'alice-dev-a7f3c9-data')})
        cloud.buckets_.append({'name': 'superserve-devbox-3f9c0a1b2c4d', 'labels': {}})
        lookalike = labels('alice-dev-a7f3c9', '3f9c0a1b2c4d')
        cloud.disks_.append({'name': 'alice-dev-a7f3c9-data2', 'zone': ZONE, 'labels': lookalike,
                             'selfLink': link('disks', 'alice-dev-a7f3c9-data2')})
        cloud.buckets_.append({'name': 'superserve-devbox-3f9c0a1b2c4d-backup', 'labels': lookalike})
        found, ignored = boxes(cloud)
        self.assertEqual(found, [])
        self.assertEqual(sorted(r.name for r, _ in ignored),
                         ['alice-dev-a7f3c9-data2', 'superserve-devbox-3f9c0a1b2c4d-backup'])

    def test_missing_or_invalid_identity_labels_are_ignored(self):
        for missing in ('devbox-id', 'devbox-name'):
            for value in (None, '', 'NOT-VALID'):
                with self.subTest(missing=missing, value=value):
                    cloud = FakeCloud()
                    cloud.add_box('alice-dev-a7f3c9', '3f9c0a1b2c4d')
                    for item in cloud.instances_ + cloud.disks_ + cloud.buckets_:
                        if value is None:
                            del item['labels'][missing]
                        else:
                            item['labels'][missing] = value
                    found, ignored = boxes(cloud)
                    self.assertEqual(found, [])
                    self.assertEqual(len(ignored), 4)

    def test_wrong_project_and_staging_discovery_labels_are_ignored(self):
        cloud = FakeCloud()
        cloud.add_box('alice-dev-a7f3c9', '3f9c0a1b2c4d', project='example-other-project', bucket=False)
        cloud.add_box('bob-dev-0d1e2f', '0d1e2f3a4b5c', sandbox_role='vmd')
        found, ignored = boxes(cloud)
        self.assertEqual(found, [])
        reasons = {r.name: reason for r, reason in ignored}
        self.assertIn(f'not in project {PROJECT}', reasons['alice-dev-a7f3c9'])
        self.assertIn('sandbox_role', reasons['bob-dev-0d1e2f'])
        self.assertEqual(len(ignored), 7)

    def test_orphan_disks_and_buckets_remain_eligible_without_vm(self):
        cloud = FakeCloud()
        cloud.add_box('alice-dev-a7f3c9', '3f9c0a1b2c4d', vm=False)
        cloud.add_box('bob-dev-0d1e2f', '0d1e2f3a4b5c', vm=False, disks=False)
        found, _ = boxes(cloud)
        self.assertEqual([(b.name, b.state) for b in found],
                         [('alice-dev-a7f3c9', 'orphaned (no VM)'), ('bob-dev-0d1e2f', 'orphaned (no VM)')])
        self.assertEqual(found[0].missing, ['instance alice-dev-a7f3c9'])
        self.assertEqual(len(found[1].resources), 1)


class SelectionTest(unittest.TestCase):
    def setUp(self):
        self.cloud = FakeCloud()
        self.cloud.add_box('alice-dev-a7f3c9', '3f9c0a1b2c4d', expires_at=NOW - 1)
        self.cloud.add_box('alice-dev-b8e4d0', '4a0d1b2c3d4e', expires_at=NOW)
        self.cloud.add_box('bob-dev-0d1e2f', '0d1e2f3a4b5c', owner='bob', expires_at=NOW + 1, vm=False)
        self.cloud.add_box('carol-dev-1f2a3b', '1f2a3b4c5d6e', owner='carol')
        for item in self.cloud.instances_ + self.cloud.disks_ + self.cloud.buckets_:
            if item['labels']['devbox-name'] == 'carol-dev-1f2a3b':
                del item['labels']['expires-at']

    def names(self, action, **kwargs):
        targets, _ = inv.plan(self.cloud, PROJECT, action, now=NOW, **kwargs)
        return [t['name'] for t in targets]

    def test_expiry_selects_passed_boxes_including_orphans_and_skips_unset(self):
        self.assertEqual(self.names('auto-delete'), ['alice-dev-a7f3c9', 'alice-dev-b8e4d0'])
        self.cloud.buckets_[2]['labels']['expires-at'] = str(NOW - 5)
        self.assertIn('bob-dev-0d1e2f', self.names('auto-delete'))

    def test_named_mine_and_list(self):
        self.assertEqual(self.names('destroy-named', name='bob-dev-0d1e2f'), ['bob-dev-0d1e2f'])
        self.assertEqual(self.names('destroy-named', name='bob-dev'), [])
        self.assertEqual(self.names('destroy-mine', actor='Alice'), ['alice-dev-a7f3c9', 'alice-dev-b8e4d0'])
        self.assertEqual(self.names('list'), [])
        with self.assertRaises(SystemExit):
            inv.plan(self.cloud, PROJECT, 'destroy-named', name='Bad_Name', now=NOW)

    def test_destroy_all_requires_confirmation_bound_to_the_exact_set(self):
        targets, report = inv.plan(self.cloud, PROJECT, 'destroy-all', now=NOW)
        self.assertEqual(targets, [])
        token = inv.set_token([b.target() for b in boxes(self.cloud)[0]])
        self.assertIn(token, report)
        self.assertEqual(len(self.names('destroy-all', confirm=token)), 4)
        self.cloud.add_box('dave-dev-2a3b4c', '2a3b4c5d6e7f', owner='dave')
        with self.assertRaises(SystemExit) as stop:
            inv.plan(self.cloud, PROJECT, 'destroy-all', confirm=token, now=NOW)
        self.assertIn('dave-dev-2a3b4c', stop.exception.code)
        with self.assertRaises(SystemExit):
            inv.plan(self.cloud, PROJECT, 'destroy-all', confirm='0' * 16, now=NOW)

    def test_token_binds_zone_and_identity_not_order(self):
        a = {'name': 'alice-dev-a7f3c9', 'zone': ZONE, 'devbox_id': '3f9c0a1b2c4d'}
        b = {'name': 'bob-dev-0d1e2f', 'zone': '', 'devbox_id': '0d1e2f3a4b5c'}
        self.assertEqual(inv.set_token([a, b]), inv.set_token([b, a]))
        self.assertNotEqual(inv.set_token([a]), inv.set_token([{**a, 'zone': 'us-east1-b'}]))
        self.assertNotEqual(inv.set_token([a]), inv.set_token([{**a, 'devbox_id': '000000000000'}]))
        self.assertNotEqual(inv.set_token([a]), inv.set_token([a, b]))


class DestroyTest(unittest.TestCase):
    def test_removes_exactly_the_box_and_marks_it_destroying_first(self):
        cloud = FakeCloud()
        cloud.add_box('alice-dev-a7f3c9', '3f9c0a1b2c4d')
        cloud.add_box('alice-dev-a7f3c9x', '5b6c7d8e9f0a')
        inv.destroy(cloud, PROJECT, 'alice-dev-a7f3c9', '3f9c0a1b2c4d')
        self.assertEqual(cloud.calls, [
            ('metadata', 'alice-dev-a7f3c9', 'destroying'), ('instance', 'alice-dev-a7f3c9'),
            ('disk', 'alice-dev-a7f3c9'), ('disk', 'alice-dev-a7f3c9-data'),
            ('bucket', 'superserve-devbox-3f9c0a1b2c4d')])
        [left], _ = boxes(cloud)
        self.assertEqual((left.name, len(left.resources)), ('alice-dev-a7f3c9x', 4))

    def test_recreated_name_with_new_identity_is_left_alone(self):
        cloud = FakeCloud()
        cloud.add_box('alice-dev-a7f3c9', '5b6c7d8e9f0a')
        self.assertIn('already gone', inv.destroy(cloud, PROJECT, 'alice-dev-a7f3c9', '3f9c0a1b2c4d'))
        self.assertEqual(cloud.calls, [])

    def test_orphans_are_removed_and_rerun_is_success(self):
        cloud = FakeCloud()
        cloud.add_box('alice-dev-a7f3c9', '3f9c0a1b2c4d', vm=False)
        inv.destroy(cloud, PROJECT, 'alice-dev-a7f3c9', '3f9c0a1b2c4d')
        self.assertEqual([c[0] for c in cloud.calls], ['disk', 'disk', 'bucket'])
        self.assertIn('already gone', inv.destroy(cloud, PROJECT, 'alice-dev-a7f3c9', '3f9c0a1b2c4d'))

    def test_partial_failure_lists_remaining_and_retry_finishes(self):
        cloud = FakeCloud()
        cloud.add_box('alice-dev-a7f3c9', '3f9c0a1b2c4d')
        cloud.fail = {('bucket', 'superserve-devbox-3f9c0a1b2c4d')}
        with self.assertRaises(SystemExit) as stop:
            inv.destroy(cloud, PROJECT, 'alice-dev-a7f3c9', '3f9c0a1b2c4d')
        self.assertIn('bucket superserve-devbox-3f9c0a1b2c4d', stop.exception.code)
        self.assertIn('permission denied', stop.exception.code)
        cloud.fail, cloud.calls = set(), []
        inv.destroy(cloud, PROJECT, 'alice-dev-a7f3c9', '3f9c0a1b2c4d')
        self.assertEqual(cloud.calls, [('bucket', 'superserve-devbox-3f9c0a1b2c4d')])

    def test_disk_attached_to_another_instance_is_not_deleted(self):
        cloud = FakeCloud()
        cloud.add_box('alice-dev-a7f3c9', '3f9c0a1b2c4d', vm=False)
        cloud.disks_[1]['users'] = [link('instances', 'staging-vmd-1')]
        with self.assertRaises(SystemExit) as stop:
            inv.destroy(cloud, PROJECT, 'alice-dev-a7f3c9', '3f9c0a1b2c4d')
        self.assertIn('attached to staging-vmd-1', stop.exception.code)
        self.assertNotIn(('disk', 'alice-dev-a7f3c9-data'), cloud.calls)

    def test_auto_delete_rechecks_expiry_before_deleting(self):
        cloud = FakeCloud()
        cloud.add_box('alice-dev-a7f3c9', '3f9c0a1b2c4d', expires_at=NOW + 60)
        self.assertIn('no longer expired', inv.destroy(cloud, PROJECT, 'alice-dev-a7f3c9', '3f9c0a1b2c4d',
                                                         require_expired_at=NOW))
        self.assertEqual(cloud.calls, [])


class RenderTest(unittest.TestCase):
    def test_list_shows_partial_boxes_revisions_and_ignored_resources(self):
        cloud = FakeCloud()
        cloud.add_box('alice-dev-a7f3c9', '3f9c0a1b2c4d', bucket=False)
        cloud.buckets_.append({'name': 'superserve-devbox-ffffffffffff', 'labels': labels('bob-dev-0d1e2f', '0d1e2f3a4b5c')})
        found, ignored = boxes(cloud)
        report = inv.render(found, ignored, NOW)
        self.assertIn('partial: missing bucket superserve-devbox-3f9c0a1b2c4d', report)
        self.assertIn('feature-x @ bbbbbbbbbbbb', report)
        self.assertIn('1d 2h', report)
        self.assertIn(contract.forwarding_command('alice-dev-a7f3c9', ZONE), report)
        self.assertIn('superserve-devbox-ffffffffffff', report)


if __name__ == '__main__':
    unittest.main()
