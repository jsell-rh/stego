#!/usr/bin/env python3
"""Check declaration limits and reject unsafe CI matrix values."""

import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('matrix', Path(__file__).with_name('application-build-matrix.py'))
control = importlib.util.module_from_spec(spec)
spec.loader.exec_module(control)


class MatrixTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.path = self.root / 'images.json'
        self.image = {'name': 'api', 'module': '.', 'target': 'out', 'entrypoint': 'service'}

    def read(self, value):
        self.path.write_text(json.dumps(value))
        return control.matrix(self.path)

    def test_multiple_modules_and_entrypoints(self):
        images = [self.image, dict(self.image, name='console', module='console'),
                  dict(self.image, name='controller', target='out/processes/controller', entrypoint='worker')]
        self.assertEqual(self.read({'format': 1, 'images': images}), {'include': images})

    def test_closed_schema(self):
        for value in [[], {}, {'format': True, 'images': [self.image]},
                      {'format': 2, 'images': [self.image]},
                      {'format': 1, 'images': [self.image], 'command': 'run'},
                      {'format': 1, 'images': [dict(self.image, command='run')]},
                      {'format': 1, 'images': [None]},
                      {'format': 1, 'images': [dict(self.image, entrypoint=None)]}]:
            with self.subTest(value=value), self.assertRaises(control.DeclarationError):
                self.read(value)
        for key in self.image:
            with self.subTest(missing=key), self.assertRaises(control.DeclarationError):
                self.read({'format': 1, 'images': [{k: v for k, v in self.image.items() if k != key}]})

    def test_duplicate_fields_and_names(self):
        self.path.write_text('{"format":1,"format":1,"images":[]}')
        with self.assertRaises(control.DeclarationError):
            control.matrix(self.path)
        self.path.write_text('{"format":1,"images":[{"name":"api","name":"api"}]}')
        with self.assertRaises(control.DeclarationError):
            control.matrix(self.path)
        with self.assertRaises(control.DeclarationError):
            self.read({'format': 1, 'images': [self.image, self.image]})

    def test_paths_and_shell_values(self):
        for key in ['module', 'target']:
            for value in ['../out', '/out', 'out/../worker', 'out//worker', 'out/', './out',
                          'out\\worker', 'out\nworker', 'out/--flag', '$(id)', '${{ secrets.TOKEN }}',
                          'out;id', '*', '', None, 5, 'a/' * 130]:
                with self.subTest(key=key, value=value), self.assertRaises(control.DeclarationError):
                    self.read({'format': 1, 'images': [dict(self.image, **{key: value})]})
        for key in ['name', 'entrypoint']:
            for value in ['../out', 'out/worker', 'out\nworker', '$(id)', '${{ secrets.TOKEN }}',
                          'out;id', '*', '', None, 5, 'x' * 65]:
                with self.subTest(key=key, value=value), self.assertRaises(control.DeclarationError):
                    self.read({'format': 1, 'images': [dict(self.image, **{key: value})]})
        with self.assertRaises(control.DeclarationError):
            self.read({'format': 1, 'images': [dict(self.image, target='.')]})

    def test_workload_and_file_limits(self):
        for images in [[], [dict(self.image, name=f'api-{n}') for n in range(65)]]:
            with self.assertRaises(control.DeclarationError):
                self.read({'format': 1, 'images': images})
        for data in ['', ' ' * ((16 << 10) + 1)]:
            self.path.write_text(data)
            with self.assertRaises(control.DeclarationError):
                control.matrix(self.path)

    def test_non_regular_inputs(self):
        self.path.write_text(json.dumps({'format': 1, 'images': [self.image]}))
        link = self.root / 'link'
        link.symlink_to(self.path)
        pipe = self.root / 'pipe'
        os.mkfifo(pipe)
        for path in [link, pipe, self.root]:
            with self.subTest(path=path), self.assertRaises((OSError, control.DeclarationError)):
                control.matrix(path)


if __name__ == '__main__':
    unittest.main()
