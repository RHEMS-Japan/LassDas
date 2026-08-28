package worker

import "testing"

// 納品先のトップレベルを全部並べても弾かれないこと。以前は 8 個で頭打ちになり、
// client/tests/ のような実在するディレクトリを列挙から外さざるを得なかった。
// その結果、画面を変えると既存テストの修正が必要になる仕事が構造的に完走しなかった。
func TestModeAcceptsEveryTopLevelDirectory(t *testing.T) {
	config := validTestConfig()
	config.Consumers[0].Mode.AllowedFilePrefixes = []string{
		"api/", "client/", "docker/", "doctor/", "docs/",
		"k8s/", "packaging/", "cloudformation/", "scripts/", "templates/",
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("10 個のディレクトリが拒否された: %v", err)
	}
}

// 空は引き続き拒否する。「どこでも書ける」は省略ではなく明示で表す。
func TestModeStillRequiresAtLeastOnePrefix(t *testing.T) {
	config := validTestConfig()
	config.Consumers[0].Mode.AllowedFilePrefixes = nil
	if err := config.Validate(); err == nil {
		t.Fatal("空のリストが通った")
	}
}

// 重複は引き続き拒否する (上限を外しても雑な列挙は許さない)。
func TestModeStillRejectsDuplicatePrefixes(t *testing.T) {
	config := validTestConfig()
	config.Consumers[0].Mode.AllowedFilePrefixes = []string{"api/", "client/", "api/"}
	if err := config.Validate(); err == nil {
		t.Fatal("重複したリストが通った")
	}
}
