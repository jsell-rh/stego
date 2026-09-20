#!/usr/bin/env python3
"""Read a bounded application image declaration for a CI build matrix."""

import argparse
import json
import os
from pathlib import Path
import re
import stat


class DeclarationError(ValueError):
    pass


def unique_object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise DeclarationError('The declaration has a repeated field')
        value[key] = item
    return value


def relative_path(value, *, allow_root=False):
    if not isinstance(value, str) or not 1 <= len(value) <= 256:
        return False
    if allow_root and value == '.':
        return True
    return all(re.fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}', part)
               for part in value.split('/'))


def matrix(path):
    # The final file must not be a link, directory, device, or pipe. The caller
    # selects the checked-out source directory and the declaration path.
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or not 0 < info.st_size <= 16 << 10:
            raise DeclarationError('The declaration must be a regular file of at most 16 KiB')
        data = stream.read((16 << 10) + 1)
        if len(data) != info.st_size or len(data) > 16 << 10:
            raise DeclarationError('The declaration changed size')
    value = json.loads(data, object_pairs_hook=unique_object)
    if not isinstance(value, dict) or set(value) != {'format', 'images'}:
        raise DeclarationError('The declaration fields are not supported')
    if type(value['format']) is not int or value['format'] != 1:
        raise DeclarationError('The declaration format is not supported')
    images = value['images']
    # This limit bounds one CI matrix. It does not limit application capacity.
    if not isinstance(images, list) or not 1 <= len(images) <= 64:
        raise DeclarationError('Select between 1 and 64 images for one CI matrix')
    names = set()
    for item in images:
        if not isinstance(item, dict) or set(item) != {'name', 'module', 'target', 'entrypoint'}:
            raise DeclarationError('An image has missing or unsupported fields')
        name = item['name']
        if not isinstance(name, str) or not re.fullmatch(r'[a-z0-9][a-z0-9-]{0,62}', name):
            raise DeclarationError('An image name must be a safe lowercase label')
        if name in names:
            raise DeclarationError('Each image name must be unique')
        names.add(name)
        if not relative_path(item['module'], allow_root=True) or not relative_path(item['target']):
            raise DeclarationError('An image module or target is not a safe relative path')
        if not isinstance(item['entrypoint'], str) or not re.fullmatch(r'[a-z][a-z0-9_-]{0,63}', item['entrypoint']):
            raise DeclarationError('An image entry point is not supported')
    return {'include': images}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--declaration', type=Path, required=True)
    args = parser.parse_args()
    try:
        result = matrix(args.declaration)
    except (OSError, ValueError, RecursionError):
        parser.exit(1, 'The application image declaration is not valid\n')
    print(json.dumps(result, separators=(',', ':')))


if __name__ == '__main__':
    main()
