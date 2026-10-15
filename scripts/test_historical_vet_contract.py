#!/usr/bin/env python3
"""Synthetic unit data only; immutable Git integration is an external witness."""
import copy
from pathlib import Path
import subprocess
import unittest
from unittest import mock

import go_cache_context
import historical_vet_contract as contract
import test_ci_tiers


class HistoricalContractTest(unittest.TestCase):
    expected = {'source': 'a' * 40, 'tree': 'b' * 40}

    def git(self, blobs, *, mode='100644'):
        def output(argv, **kwargs):
            self.assertEqual(argv[:3], ['git', '-C', '/synthetic'])
            args = argv[3:]
            if args == ['rev-parse', '--verify', self.expected['source'] + '^{commit}']:
                return (self.expected['source'] + '\n').encode()
            if args == ['rev-parse', self.expected['source'] + '^{tree}']:
                return (self.expected['tree'] + '\n').encode()
            self.assertEqual(args[:3], ['ls-tree', self.expected['source'], '--'])
            index = contract.PATHS.index(args[3])
            return (mode + ' blob ' + blobs[index] + '\t' + args[3] + '\n').encode()
        return output

    def context(self):
        value = copy.deepcopy(test_ci_tiers.PlainVetTest.context)
        value = {k: v for k, v in value.items() if k in contract.FIELDS}
        value['go'] = {k: v for k, v in value['go'].items() if k in contract.GO_FIELDS}
        value['schema'] = 'aa-pr-go-v1'
        return value

    def test_both_recognized_contracts_validate_original_data(self):
        for blobs in contract.CONTRACTS:
            with self.subTest(blobs=blobs), mock.patch.object(subprocess, 'check_output', side_effect=self.git(blobs)):
                token = contract.resolve(Path('/synthetic'), self.expected)
                value = self.context(); before = copy.deepcopy(value)
                self.assertIs(contract.validate_context(value, self.expected, token, '/synthetic'), value)
                self.assertEqual(value, before)
                with self.assertRaises(go_cache_context.InvalidContext): go_cache_context.validate_context(value)

    def test_unknown_mode_blob_and_proof_refuse_authority(self):
        blobs = next(iter(contract.CONTRACTS))
        for wrong in (('0' * 40,) + blobs[1:], tuple(reversed(blobs))):
            with mock.patch.object(subprocess, 'check_output', side_effect=self.git(wrong)):
                self.assertIsNone(contract.resolve('/synthetic', self.expected))
        with mock.patch.object(subprocess, 'check_output', side_effect=self.git(blobs, mode='120000')):
            self.assertIsNone(contract.resolve('/synthetic', self.expected))
        for expected in ({'source': 'HEAD', 'tree': 'b' * 40}, dict(self.expected, extra='bad')):
            with self.assertRaises(go_cache_context.InvalidContext): contract.resolve('/synthetic', expected)
        with mock.patch.object(subprocess, 'check_output', side_effect=subprocess.CalledProcessError(128, ['git'])):
            with self.assertRaises(go_cache_context.InvalidContext): contract.resolve('/synthetic', self.expected)

    def test_schema_downgrade_missing_extra_hash_source_and_token_refuse(self):
        blobs = next(iter(contract.CONTRACTS))
        with mock.patch.object(subprocess, 'check_output', side_effect=self.git(blobs)):
            token = contract.resolve('/synthetic', self.expected)
            changes = (lambda c: c.update(schema='aa-pr-go-v2'), lambda c: c.update(extra='bad'),
                       lambda c: c['go'].pop('GOAMD64'), lambda c: c['go'].update(GOCACHEPROG=''),
                       lambda c: c['toolchain'].update(vet='bad'), lambda c: c.update(race=True))
            for mutate in changes:
                value = self.context(); mutate(value)
                with self.assertRaises(go_cache_context.InvalidContext):
                    contract.validate_context(value, self.expected, token, '/synthetic')
            for bad_token, expected in ((None, self.expected),
                    (contract.HistoricalContract(token.source, token.tree, tuple(reversed(blobs))), self.expected),
                    (token, dict(self.expected, tree='c' * 40)),
                    (token, dict(self.expected, source='c' * 40))):
                with self.assertRaises(go_cache_context.InvalidContext):
                    contract.validate_context(self.context(), expected, bad_token, '/synthetic')


if __name__ == '__main__': unittest.main()
