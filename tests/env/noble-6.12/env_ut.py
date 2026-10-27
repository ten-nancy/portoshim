import os


class TestEnv():
    def test_env(self):
        major, minor, *_ = tuple(os.uname().release.split('.'))
        assert (major, minor) == ('6', '12')
