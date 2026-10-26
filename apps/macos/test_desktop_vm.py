import contextlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

import desktop_vm as vm

META = {"xcode": "27.0", "sdk": "27.0", "minimum_macos": 27}


def prepared(config, staging, mode, configuration, assets):
    runtime = staging / "runtime"
    runtime.mkdir()
    (runtime / "prepare-display").write_text("prepared executable")
    return META


class Fixture:
    def __init__(self, directory):
        self.directory = Path(directory)
        self.settings = self.directory / "settings.json"
        self.settings.write_text("{}")
        self.home = self.directory / "home"
        self.root = self.directory / "project"
        self.root.mkdir()
        self.tart = self.directory / "tart"
        self.tart.write_text("#!/usr/bin/env python3\n" + r'''
import json, os, pathlib, signal, sys, time
root=pathlib.Path(ROOT_DIRECTORY)
settings=json.loads((root/'settings.json').read_text())
arguments=sys.argv[1:]
fd=os.open(root/'calls.jsonl',os.O_WRONLY|os.O_CREAT|os.O_APPEND,0o600)
os.write(fd,(json.dumps({'args':arguments,'environment':dict(os.environ),'pid':os.getpid()})+'\n').encode());os.close(fd)
name='albert-desktop-development'
vm=pathlib.Path(os.environ['TART_HOME'])/'vms'/name
command=arguments[0]
if command=='list':
 entries=[{'Name':'ghcr.io/cirruslabs/macos-golden-gate-xcode:27','Source':'OCI'},
 {'Name':'ghcr.io/cirruslabs/macos-golden-gate-xcode@sha256:'+'a'*64,'Source':'OCI'},
 {'Name':'polka-desktop-tests','Source':'local','Running':True}]
 if vm.exists():entries.append({'Name':name,'Source':'local','Running':settings.get('running',False)})
 print(json.dumps(entries));sys.exit(0)
if command=='clone':vm.mkdir(parents=True);sys.exit(settings.get('clone_failure',0))
if command=='get':print(json.dumps({'Running':settings.get('running',False)}));sys.exit(0)
if command=='set':sys.exit(0)
if command=='run':
 (root/'vm.pid').write_text(str(os.getpid()))
 for argument in arguments:
  if argument.startswith('--dir=albert-output:'):(root/'output.path').write_text(argument.split('--dir=albert-output:',1)[1])
 def stopped(signum,frame):
  (root/'stopped').write_text(str(signum));sys.exit(0)
 signal.signal(signal.SIGTERM,stopped);signal.signal(signal.SIGINT,stopped)
 while True:time.sleep(.02)
if command=='exec':
 if settings.get('plant_log_symlink'):
  output=pathlib.Path((root/'output.path').read_text())
  host_log=output.parent/'doctor.log'
  if not host_log.exists():host_log.symlink_to(root/'canary')
 if arguments[2:4]==['/bin/sh','-c']:
  if 'ALBERT_GUEST_READY' in arguments[4]:
   counter=root/'probes'
   count=int(counter.read_text()) if counter.exists() else 0
   counter.write_text(str(count+1))
   if settings.get('all_probes_hang') or count<settings.get('hang_probes',0):
    with (root/'probe.pids').open('a') as stream:stream.write(str(os.getpid())+'\n')
    while True:time.sleep(.02)
   print('ALBERT_GUEST_READY');sys.exit(0)
  print('prepared guest display');sys.exit(0)
 if settings.get('guest_hangs'):
  (root/'guest.started').write_text('started')
  while True:time.sleep(.02)
 output=pathlib.Path((root/'output.path').read_text())
 print('ALBERT_SMOKE_PASS')
 print("Test Suite 'InterfaceTests' passed")
 print('Executed 9 tests, with 0 failures')
 print('ALBERT_XCTEST_BUNDLE_PASS AlbertCoreTests.xctest')
 if not settings.get('missing_ui'):print('ALBERT_XCTEST_BUNDLE_PASS AlbertUITests.xctest')
 for mode in ('doctor','smoke','test','screenshots','run'):print('ALBERT_VM_GUEST_PASS '+mode)
 sys.exit(settings.get('failure',0))
sys.exit(97)
'''.replace("ROOT_DIRECTORY", repr(str(self.directory))))
        self.tart.chmod(0o700)
        self.env = dict(os.environ, ALBERT_TART=str(self.tart), TART_HOME=str(self.directory / "tart-home"),
                        ALBERT_VM_STATE=str(self.directory / "state"), SPARKLE_PRIVATE_KEY="must-not-cross", DESKTOP_API_TOKEN="must-not-cross")
        self.config = vm.Configuration(self.env, home=self.home, root=self.root)

    def change(self, **changes):
        self.settings.write_text(json.dumps(changes))

    def calls(self):
        path = self.directory / "calls.jsonl"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def setup(self):
        with contextlib.redirect_stdout(None): vm.setup(self.config, lock_timeout=1)

    def check_stopped(self, case):
        pid = int((self.directory / "vm.pid").read_text())
        with case.assertRaises(ProcessLookupError): os.kill(pid, 0)
        case.assertTrue((self.directory / "stopped").exists())
        case.assertFalse(any(call["args"][0] in {"stop", "delete"} for call in self.calls()))


class ConfigurationTests(unittest.TestCase):
    def test_runtime_archive_preserves_framework_links_on_guest_disk(self):
        with tempfile.TemporaryDirectory() as directory:
            staging = Path(directory)
            framework = staging / "runtime/Sparkle.framework"
            version = framework / "Versions/B"
            version.mkdir(parents=True)
            (version / "Sparkle").write_bytes(b"prepared binary")
            (framework / "Versions/Current").symlink_to("B")
            (framework / "Sparkle").symlink_to("Versions/Current/Sparkle")
            vm.archive_runtime(staging)
            guest = staging / "guest"
            guest.mkdir()
            subprocess.run(["/usr/bin/tar", "-xf", str(staging / "runtime.tar"), "-C", str(guest)], check=True)
            binary = guest / "Sparkle.framework/Sparkle"
            self.assertTrue(binary.is_symlink())
            self.assertEqual(binary.read_bytes(), b"prepared binary")
            self.assertEqual(os.readlink(guest / "Sparkle.framework/Versions/Current"), "B")

    def test_snapshot_contains_only_reviewable_sources_and_no_secrets(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "project"; root.mkdir()
            files = {"apps/macos/Sources/UI/Edited.swift":"current edited source", "apps/macos/Tests/NewTests.swift":"untracked test", "apps/macos/Package.swift":"manifest",
                     ".env":"private", ".git/config":"credentials", "apps/macos/.build/cache":"build database", "apps/macos/Fixtures/other.jpg":"private fixture",
                     "apps/macos/Sources/.env.local":"private", "apps/macos/Tests/private.p12":"certificate", "apps/macos/Tests/profile.sqlite":"profile"}
            for name, data in files.items():
                path=root/name;path.parent.mkdir(parents=True,exist_ok=True);path.write_text(data)
            output=Path(directory)/"snapshot";vm.source_snapshot(root,output)
            names={str(path.relative_to(output)) for path in output.rglob('*') if path.is_file()}
            self.assertEqual(names,{"apps/macos/Sources/UI/Edited.swift","apps/macos/Tests/NewTests.swift","apps/macos/Package.swift"})
            (root/"apps/macos/Sources/Secret.swift").symlink_to(root/".env")
            with self.assertRaises(vm.VMError):vm.source_snapshot(root,Path(directory)/"unsafe")

    def test_guest_script_never_compiles_and_uses_clean_explicit_backend(self):
        for mode in ("doctor","test","smoke","screenshots","run"):
            script=vm.guest_script(mode,"a"*32)
            self.assertNotIn("export HOME",script)
            self.assertNotIn("swiftc",script);self.assertNotIn("swift build",script);self.assertNotIn("swift test",script)
            self.assertIn("ALBERT_DESKTOP_BACKEND=guest",script)
            result=subprocess.run(["/bin/bash","-n"],input=script.encode(),capture_output=True)
            self.assertEqual(result.returncode,0,result.stderr)
        self.assertNotIn("SPARKLE_PRIVATE_KEY",vm.safe_environment({"SPARKLE_PRIVATE_KEY":"secret"}))

    def test_invalid_timeouts_and_log_links_fail_closed(self):
        for value in (float('nan'), float('inf'), -1, 0):
            with self.assertRaises(vm.VMError): vm.finite_timeout(value)
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);canary=root/'canary';canary.write_text('private host data')
            link=root/'log';link.symlink_to(canary)
            with self.assertRaises(OSError):
                with vm.secure_log(link,truncate=True) as log:log.write(b'overwrite')
            link.unlink();os.link(canary,link)
            with self.assertRaises(vm.VMError):
                with vm.secure_log(link,truncate=True) as log:log.write(b'overwrite')
            self.assertEqual(canary.read_text(),'private host data')

    def test_global_lock_does_not_depend_on_worktree_or_vm_name(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture=Fixture(directory)
            other=vm.Configuration(dict(fixture.env,ALBERT_VM_NAME="albert-other",ALBERT_VM_STATE=str(Path(directory)/"other-state")),home=fixture.home,root=fixture.root/'other-worktree')
            self.assertEqual(fixture.config.lock,other.lock)
            with vm.global_lock(fixture.config.lock,.1):
                with self.assertRaises(vm.VMError):
                    with vm.global_lock(other.lock,.05):pass
            self.assertTrue(fixture.config.lock.exists())
            with vm.global_lock(other.lock,.1):pass

    def test_setup_uses_cached_digest_and_never_touches_polka(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture=Fixture(directory);fixture.setup();fixture.setup()
            calls=fixture.calls()
            clones=[call['args'] for call in calls if call['args'][0]=='clone']
            self.assertEqual(clones,[['clone','ghcr.io/cirruslabs/macos-golden-gate-xcode@sha256:'+'a'*64,vm.DEFAULT_NAME]])
            self.assertFalse(any(call['args'][0]=='run' for call in calls))
            for call in calls:
                self.assertNotIn('DESKTOP_API_TOKEN',call['environment']);self.assertNotIn('SPARKLE_PRIVATE_KEY',call['environment'])
                self.assertEqual(call['environment']['TART_NO_AUTO_PRUNE'],'1')
            marker=json.loads(fixture.config.owner.read_text());self.assertEqual(marker['directory_identity'],vm.vm_identity(fixture.config))
            fixture.config.owner.unlink()
            with self.assertRaises(vm.VMError):fixture.setup()
            self.assertEqual(len([call for call in fixture.calls() if call['args'][0]=='clone']),1)


class LifecycleTests(unittest.TestCase):
    def test_success_and_failed_guest_are_reaped_and_preserve_diagnostics(self):
        for failure in (0,7):
            with self.subTest(failure=failure),tempfile.TemporaryDirectory() as directory:
                fixture=Fixture(directory);fixture.setup();fixture.change(failure=failure)
                with contextlib.redirect_stdout(None):
                    if failure:
                        with self.assertRaises(vm.VMError) as error:vm.run_prepared(fixture.config,'test',prepare=prepared,timeout=1,readiness_timeout=1,probe_timeout=.2)
                        self.assertEqual(error.exception.status,failure)
                    else:vm.run_prepared(fixture.config,'test',prepare=prepared,timeout=1,readiness_timeout=1,probe_timeout=.2)
                fixture.check_stopped(self)
                output=next((fixture.root/'apps/macos/build/vm').iterdir());result=json.loads((output/'result.json').read_text())
                self.assertEqual(result['ok'],failure==0);self.assertTrue((output/'tests.log').exists())
                call=next(call['args'] for call in fixture.calls() if call['args'][0]=='run')
                for option in ('--no-graphics','--no-audio','--no-clipboard','--no-usb-accessories'):self.assertIn(option,call)
                self.assertTrue(any(arg.startswith('--dir=albert-input:') and arg.endswith(':ro') for arg in call))
                self.assertTrue(any(arg.startswith('--dir=albert-output:') for arg in call))
                with vm.global_lock(fixture.config.lock,.1):pass

    def test_running_unowned_or_replaced_vm_is_never_taken_over(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture=Fixture(directory);fixture.setup();fixture.change(running=True)
            with contextlib.redirect_stdout(None),self.assertRaises(vm.VMError):vm.run_prepared(fixture.config,'smoke',prepare=prepared)
            self.assertFalse((fixture.directory/'vm.pid').exists())
            fixture.change();old=fixture.config.vm_directory.with_name('previous');fixture.config.vm_directory.rename(old);fixture.config.vm_directory.mkdir()
            with self.assertRaises(vm.VMError):vm.require_owned(fixture.config)
            self.assertFalse(any(call['args'][0] in ('stop','delete') for call in fixture.calls()))

    def test_readiness_probe_timeout_retries_and_reaps_each_probe(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture=Fixture(directory);fixture.setup();fixture.change(hang_probes=1)
            with contextlib.redirect_stdout(None):vm.run_prepared(fixture.config,'smoke',prepare=prepared,timeout=1,readiness_timeout=2,probe_timeout=.15)
            fixture.check_stopped(self)
            for pid in (fixture.directory/'probe.pids').read_text().splitlines():
                with self.assertRaises(ProcessLookupError):os.kill(int(pid),0)

    def test_timeout_stops_owned_vm_and_saves_failure(self):
        for settings in ({'all_probes_hang':True},{'guest_hangs':True}):
            with self.subTest(settings=settings),tempfile.TemporaryDirectory() as directory:
                fixture=Fixture(directory);fixture.setup();fixture.change(**settings)
                with contextlib.redirect_stdout(None),self.assertRaises(vm.VMError):vm.run_prepared(fixture.config,'smoke',prepare=prepared,timeout=.15,readiness_timeout=.4,probe_timeout=.1)
                fixture.check_stopped(self)
                result=json.loads(next((fixture.root/'apps/macos/build/vm').glob('*/result.json')).read_text());self.assertFalse(result['ok'])

    def test_core_only_results_cannot_pass_native_validation(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture=Fixture(directory);fixture.setup();fixture.change(missing_ui=True)
            with contextlib.redirect_stdout(None),self.assertRaisesRegex(vm.VMError,'both bundles'):vm.run_prepared(fixture.config,'test',prepare=prepared,readiness_timeout=1)
            fixture.check_stopped(self)

    def test_host_build_failure_never_starts_vm_and_retains_build_log(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture=Fixture(directory);fixture.setup()
            def fail(config,staging,*args):
                (staging/'host-build.log').write_text('compiler failure diagnostics')
                raise vm.VMError('build failed',9)
            with contextlib.redirect_stdout(None),self.assertRaises(vm.VMError):vm.run_prepared(fixture.config,'test',prepare=fail)
            output=next((fixture.root/'apps/macos/build/vm').iterdir())
            self.assertEqual((output/'host-build.log').read_text(),'compiler failure diagnostics')
            self.assertFalse((fixture.directory/'vm.pid').exists())
            with vm.global_lock(fixture.config.lock,.1):pass

    def test_guest_log_symlink_cannot_overwrite_host_file_and_vm_stops(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture=Fixture(directory);fixture.setup();fixture.change(plant_log_symlink=True)
            canary=fixture.directory/'canary';canary.write_text('private host data')
            with contextlib.redirect_stdout(None),self.assertRaises(OSError):
                vm.run_prepared(fixture.config,'smoke',prepare=prepared,readiness_timeout=1)
            self.assertEqual(canary.read_text(),'private host data');fixture.check_stopped(self)
            run=next(call['args'] for call in fixture.calls() if call['args'][0]=='run')
            output=next(argument for argument in run if argument.startswith('--dir=albert-output:'))
            self.assertTrue(output.endswith('/guest'))

    def test_cancellation_immediately_after_registered_transport_spawn_reaps_child(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture=Fixture(directory);fixture.setup();children=[];popen=subprocess.Popen
            def track(*args,**kwargs):
                child=popen(*args,**kwargs);children.append(child);return child
            with mock.patch('desktop_vm.subprocess.Popen',side_effect=track),mock.patch('desktop_vm.time.monotonic',side_effect=vm.Cancelled()),self.assertRaises(vm.Cancelled):
                vm.execute([fixture.tart,'run',vm.DEFAULT_NAME],env=fixture.config.environment)
            self.assertEqual(len(children),1);self.assertIsNotNone(children[0].poll())
            with self.assertRaises(ProcessLookupError):os.kill(children[0].pid,0)

    def test_cancellation_after_registered_vm_spawn_reaps_before_enter_returns(self):
        class InterruptedLease(vm.GuestLease):
            @property
            def process(self):return getattr(self,'stored_process',None)
            @process.setter
            def process(self,value):
                self.stored_process=value
                if value is not None:raise vm.Cancelled()
        with tempfile.TemporaryDirectory() as directory:
            fixture=Fixture(directory);fixture.setup();staging=fixture.directory/'input';staging.mkdir();output=fixture.directory/'output';output.mkdir()
            lease=InterruptedLease(fixture.config,staging,output)
            with self.assertRaises(vm.Cancelled):lease.__enter__()
            self.assertIsNotNone(lease.process.poll())
            with self.assertRaises(ProcessLookupError):os.kill(lease.process.pid,0)

    def test_signal_cancellation_reaps_vm_and_transport_before_return(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture=Fixture(directory);fixture.setup();fixture.change(guest_hangs=True)
            worker=fixture.directory/'worker.py'
            worker.write_text('import sys\nsys.path.insert(0,'+repr(str(vm.APP_ROOT))+')\nimport desktop_vm as vm\nfrom test_desktop_vm import prepared\nconfig=vm.Configuration('+repr(fixture.env)+',home='+repr(str(fixture.home))+',root='+repr(str(fixture.root))+')\nwith vm.cancellation_signals():\n vm.run_prepared(config,"smoke",prepare=prepared,readiness_timeout=2)\n')
            process=subprocess.Popen([sys.executable,str(worker)],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
            try:
                deadline=time.monotonic()+3
                while not (fixture.directory/'guest.started').exists() and time.monotonic()<deadline:time.sleep(.02)
                self.assertTrue((fixture.directory/'guest.started').exists())
                process.send_signal(signal.SIGTERM);self.assertNotEqual(process.wait(timeout=3),0)
                fixture.check_stopped(self)
                with vm.global_lock(fixture.config.lock,.1):pass
            finally:
                if process.poll() is None:process.kill();process.wait()


if __name__=='__main__':unittest.main()
