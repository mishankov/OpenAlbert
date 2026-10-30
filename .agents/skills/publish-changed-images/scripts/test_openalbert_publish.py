import io
import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location('publisher', Path(__file__).with_name('openalbert_publish.py'))
publisher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(publisher)


class SelectionTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.repo = Path(self.temporary.name)
        self.git('init', '-q')
        self.write('docs/observability.md', 'old docs')
        self.write('apps/backend/main.go', 'old backend')
        self.write('services/backup/main.go', 'old backup')
        self.old = self.commit()

    def git(self, *args):
        return subprocess.check_output(['git', '-c', 'user.name=Skill Test', '-c', 'user.email=skill@example.invalid',
                                        *args], cwd=self.repo, text=True).strip()

    def write(self, path, text):
        destination = self.repo / path
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(text)

    def commit(self):
        self.git('add', '.')
        self.git('commit', '-qm', 'Synthetic inputs')
        return self.git('rev-parse', 'HEAD')

    def inspect(self, repo, prefix, name):
        return {'reference': prefix + '/' + name + ':master', 'manifest': {'component': name},
                'platforms': {arch: {'revision': self.old, 'digest': 'sha256:synthetic'} for arch in publisher.ARCHES}}

    def make_plan(self, inspect=None):
        with mock.patch.object(publisher, 'published', side_effect=inspect or self.inspect):
            return publisher.plan(self.repo, 'registry.example/project')

    def test_backend_and_docs_select_only_backend(self):
        self.write('apps/backend/main.go', 'new backend')
        self.write('docs/observability.md', 'new docs')
        self.commit()
        self.assertEqual(self.make_plan()['selected'], ['openalbert'])

    def test_shared_backup_code_selects_backend_and_backup(self):
        self.write('services/backup/main.go', 'new backup')
        self.commit()
        self.assertEqual(self.make_plan()['selected'], ['openalbert', 'backup'])

    def test_compares_each_components_published_revision(self):
        self.write('apps/backend/main.go', 'new backend')
        current = self.commit()
        def inspect(repo, prefix, name):
            data = self.inspect(repo, prefix, name)
            if name == 'openalbert':
                for state in data['platforms'].values():
                    state['revision'] = current
            return data
        self.assertEqual(self.make_plan(inspect)['selected'], [])

    def test_one_outdated_platform_rebuilds_both(self):
        self.write('apps/backend/main.go', 'new backend')
        current = self.commit()
        def inspect(repo, prefix, name):
            data = self.inspect(repo, prefix, name)
            data['platforms']['arm64']['revision'] = current
            return data
        self.assertEqual(self.make_plan(inspect)['selected'], ['openalbert'])

    def test_non_commit_revision_is_not_passed_as_git_option(self):
        def inspect(repo, prefix, name):
            data = self.inspect(repo, prefix, name)
            if name == 'stt':
                data['platforms']['amd64']['revision'] = '--output=bad'
            return data
        self.assertEqual(self.make_plan(inspect)['selected'], ['stt'])
        self.assertFalse((self.repo / 'bad').exists())

    def test_documentation_only_does_not_select_images(self):
        self.write('docs/observability.md', 'new docs')
        self.commit()
        self.assertEqual(self.make_plan()['selected'], [])

    def test_plan_only_never_executes_a_publication(self):
        self.write('apps/backend/main.go', 'new backend')
        self.commit()
        data = self.make_plan()
        with mock.patch.object(publisher, 'plan', return_value=data), \
             mock.patch.object(publisher, 'execute') as execute, \
             mock.patch('sys.argv', ['publisher', '--repo', str(self.repo)]), \
             mock.patch('sys.stdout', new_callable=io.StringIO):
            self.assertEqual(publisher.main(), 0)
            execute.assert_not_called()

    def test_execute_with_no_changed_images_never_publishes(self):
        data = self.make_plan()
        with mock.patch.object(publisher, 'plan', return_value=data), \
             mock.patch.object(publisher, 'execute') as execute, \
             mock.patch('sys.argv', ['publisher', '--repo', str(self.repo), '--execute']), \
             mock.patch('sys.stdout', new_callable=io.StringIO):
            self.assertEqual(publisher.main(), 0)
            execute.assert_not_called()

    def test_missing_platform_fails_instead_of_silent_skip(self):
        with mock.patch.object(publisher, 'command', return_value='{}'):
            with self.assertRaisesRegex(RuntimeError, 'missing or ambiguous linux/amd64'):
                publisher.published(self.repo, 'registry.example/project', 'openalbert')

    def test_verification_detects_changed_unselected_manifest(self):
        data = self.make_plan()
        def inspect(repo, prefix, name):
            result = self.inspect(repo, prefix, name)
            if name == 'backup':
                result['manifest'] = {'unexpected': 'new image'}
            return result
        with mock.patch.object(publisher, 'published', side_effect=inspect):
            with self.assertRaisesRegex(RuntimeError, 'unchanged component manifest changed'):
                publisher.verify(self.repo, data)


if __name__ == '__main__':
    unittest.main()
