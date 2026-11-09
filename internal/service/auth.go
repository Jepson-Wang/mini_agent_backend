package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/emailx"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/hash"
	"github.com/Jepson-Wang/mini_agent_backend/internal/repository"
)

// TokenIssuer 签发访问令牌。
//
// 抽成接口是为了让 service 不认识 JWT:换成"往 sessions 表插一行再返回随机串"
// 的实现,这一层一行都不用改。测试里也不用真造一个 32 字节密钥。
// 生产实现是 pkg/token.Manager。
type TokenIssuer interface {
	Issue(userID int64) (token string, expiresAt time.Time, err error)
}

// TokenRevoker 吊销一个访问令牌。
//
// 生产实现是 pkg/revoke.Revoker。为 nil 时登出退化成"什么也不做" ——
// 那个部署没启用吊销,令牌还能用到过期。
type TokenRevoker interface {
	// RevokeToken 把 jti 加进黑名单。exp 是令牌的过期时刻,
	// 黑名单的 TTL 取 exp - now。
	RevokeToken(ctx context.Context, jti string, exp time.Time) error
}

// Credential 是一份登录态:用户本人 + 一个能代表他的令牌。
type Credential struct {
	User      *model.User
	Token     string
	ExpiresAt time.Time
}

// statusActive 是允许登录的账号状态,和 model.User.Status 的默认值一致。
const statusActive = "active"

// AuthService 负责注册、登录和登出。
type AuthService struct {
	repo   *repository.Repository
	tokens TokenIssuer

	// revoker 吊销令牌。**为 nil 时登出不做任何事** ——
	// 那个部署没启用吊销功能(没配 Redis),令牌还能用到过期。
	revoker TokenRevoker
}

func NewAuthService(repo *repository.Repository, tokens TokenIssuer, revoker TokenRevoker) *AuthService {
	return &AuthService{repo: repo, tokens: tokens, revoker: revoker}
}

// Logout 吊销当前这一个令牌。
//
// **只吊销当前令牌,不动该用户的其它令牌。**
// 用户在手机上还登着,不该被这一次的登出踢下线 ——
// 整用户吊销是"改密码"和"禁用账号"才该做的事。
//
// revoker 为 nil(没启用吊销)时这是个**成功的空操作**:
// 前端照样清掉本地 token,服务端这边没有可吊销的东西。
// 返回 error 反而会让前端弹一个"登出失败",而实际上什么都没出错。
func (s *AuthService) Logout(ctx context.Context, jti string, exp time.Time) error {
	if s == nil || s.revoker == nil {
		return nil
	}
	if jti == "" {
		// 取不到 jti 说明这个校验器没实现 ClaimsVerifier,
		// 也就是拿不到吊销的凭证。这不是错误 —— 只是这个部署没法吊销。
		slog.Warn("登出时取不到 jti,无法吊销令牌")
		return nil
	}
	if err := s.revoker.RevokeToken(ctx, jti, exp); err != nil {
		return fmt.Errorf("service: 吊销令牌失败: %w", err)
	}
	return nil
}

// Register 注册一个新用户,并直接返回登录态。
//
// 注册完直接给令牌是产品决定:少一次跳登录页。代价是这条路径上没有
// 邮箱验证 —— 要加验证的话,应该是先建账号(status 置为 pending)、
// 不签令牌,等验证完再放行。
func (s *AuthService) Register(ctx context.Context, email, password, nickname string) (*Credential, error) {
	email = normalizeEmail(email)

	// 先散列再建记录。顺序反过来的话,散列失败就会在库里留一条
	// 没有口令的用户,而唯一索引会让他再也注册不上。
	pwdHash, err := hash.Hash(password)
	if err != nil {
		// err 里不含口令本身(见 pkg/hash),但也不往上抛细节,当内部错误处理。
		return nil, fmt.Errorf("service: 生成口令散列失败: %w", err)
	}

	u, err := s.repo.User.Create(ctx, email, pwdHash, normalizeNickname(nickname))
	if err != nil {
		// 邮箱占用已经被仓库层翻译成 model.ErrEmailOccupied,
		// 对外的文案是"邮箱或密码不可用",不说到底哪个不可用。
		return nil, err
	}

	return s.issue(u)
}

// Login 校验口令并返回登录态。
//
// 失败一律返回 model.ErrInvalidCredentials —— 邮箱不存在和口令不对
// 必须是同一个错误、同一个码、同一句文案,否则这个接口就是个账号枚举器。
func (s *AuthService) Login(ctx context.Context, email, password string) (*Credential, error) {
	email = normalizeEmail(email)

	u, err := s.repo.User.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, model.ErrUserNotFound) {
			// 已知缺口:这条分支比"口令错"少跑一次 bcrypt(约 160ms),
			// 时间差能被用来判断邮箱是否注册过。要堵的话,在这里对一个
			// 固定的假散列跑一次 hash.Verify,把两条路径的耗时拉平。
			// 当前没做 —— 登录接口还没上限流,能测时间差的人也能直接爆破。
			return nil, model.ErrInvalidCredentials
		}
		return nil, err
	}

	if err := hash.Verify(u.PasswordHash, password); err != nil {
		if errors.Is(err, hash.ErrMismatchedPassword) {
			return nil, model.ErrInvalidCredentials
		}
		// ErrInvalidHash:库里那条散列是坏的(被手改过、迁移时截断过)。
		// 这不是用户的错,当内部错误抛上去,让它进日志有人管。
		return nil, fmt.Errorf("service: 用户 %d 的口令散列不可用: %w", u.ID, err)
	}

	// 状态检查放在口令校验**之后**。放前面的话,不知道口令也能问出
	// "这个邮箱存在但被禁用了",等于免费的枚举接口。
	if u.Status != statusActive {
		return nil, fmt.Errorf("%w: status=%s", model.ErrUserDisabled, u.Status)
	}

	s.afterLogin(ctx, u, password)

	return s.issue(u)
}

// afterLogin 处理登录成功后的附带写入。
//
// 全部是 best-effort:这些都失败不了业务,登录本身已经成了,
// 因为记不上时间就把用户挡在门外是本末倒置。失败只记日志。
func (s *AuthService) afterLogin(ctx context.Context, u *model.User, password string) {
	// 散列参数升级。cost 调高之后,老账号的散列只能趁用户手上正好有明文的
	// 这一刻重算 —— 散列不可逆,离线批量升级做不到。
	if hash.NeedsRehash(u.PasswordHash) {
		if newHash, err := hash.Hash(password); err != nil {
			slog.Warn("重算口令散列失败", "user_id", u.ID, "error", err)
		} else if err := s.repo.User.UpdatePasswordHash(ctx, u.ID, newHash); err != nil {
			slog.Warn("写入新口令散列失败", "user_id", u.ID, "error", err)
		} else {
			u.PasswordHash = newHash
		}
	}

	now := time.Now()
	if err := s.repo.User.TouchLastLogin(ctx, u.ID, now); err != nil {
		slog.Warn("更新最后登录时间失败", "user_id", u.ID, "error", err)
		return
	}
	u.LastLoginAt = &now
}

// issue 给用户签一个令牌。
func (s *AuthService) issue(u *model.User) (*Credential, error) {
	if s.tokens == nil {
		// 接线错误。返回 error 而不是 panic,由上层统一变成 500。
		return nil, errors.New("service: 未注入 TokenIssuer")
	}
	tok, expiresAt, err := s.tokens.Issue(u.ID)
	if err != nil {
		return nil, fmt.Errorf("service: 签发令牌失败: %w", err)
	}
	return &Credential{User: u, Token: tok, ExpiresAt: expiresAt}, nil
}

// normalizeEmail 统一邮箱的书写形式。
//
// 真正的实现已经挪到 emailx.Normalize:登录限流器的桶键要和这里查库用
// 同一套规范化,否则换个大小写就能绕过限流。这里留一层薄包装,
// 是为了让包内的调用点和测试继续用原来的名字。
func normalizeEmail(email string) string {
	return emailx.Normalize(email)
}

// normalizeNickname 去空白;空的返回 nil,对应库里的 NULL。
// 不这么做的话库里会同时存在 NULL 和 "" 两种"没有昵称",查询时得写两个条件。
func normalizeNickname(nickname string) *string {
	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		return nil
	}
	return &nickname
}
