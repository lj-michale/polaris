/**
 * Tencent is pleased to support the open source community by making Polaris available.
 *
 * Copyright (C) 2019 THL A29 Limited, a Tencent company. All rights reserved.
 *
 * Licensed under the BSD 3-Clause License (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * https://opensource.org/licenses/BSD-3-Clause
 *
 * Unless required by applicable law or agreed to in writing, software distributed
 * under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
 * CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 */

package sqldb

import (
	"database/sql"
	"fmt"
	"time"

	api "github.com/polarismesh/polaris-server/common/api/v1"
	logger "github.com/polarismesh/polaris-server/common/log"
	"github.com/polarismesh/polaris-server/common/model"
	commontime "github.com/polarismesh/polaris-server/common/time"
	"github.com/polarismesh/polaris-server/common/utils"
	"github.com/polarismesh/polaris-server/store"
	"go.uber.org/zap"
)

var (
	// 用户查询相关属性对应关系
	userAttributeMapping map[string]string = map[string]string{
		OwnerAttribute:   "u.owner",
		NameAttribute:    "u.name",
		GroupIDAttribute: "group_id",
	}

	// 用户-用户组关系查询属性对应关系
	userLinkGroupAttributeMapping map[string]string = map[string]string{
		"user_id":    "ul.user_id",
		"group_name": "ug.name",
		"user_name":  "u.name",
	}
)

type userStore struct {
	master *BaseDB
	slave  *BaseDB
}

// AddUser 添加用户
func (u *userStore) AddUser(user *model.User) error {
	if user.ID == "" || user.Name == "" || user.Token == "" || user.Password == "" {
		return store.NewStatusError(store.EmptyParamsErr, fmt.Sprintf(
			"add user missing some params, id is %s, name is %s", user.ID, user.Name))
	}

	// 先清理无效数据
	if err := u.cleanInValidUser(user.Name); err != nil {
		return err
	}

	err := RetryTransaction("addUser", func() error {
		return u.addUser(user)
	})

	return store.Error(err)
}

func (u *userStore) addUser(user *model.User) error {

	tx, err := u.master.Begin()
	if err != nil {
		return err
	}

	defer func() { _ = tx.Rollback() }()

	addSql := "INSERT INTO user(`id`, `name`, `password`, `owner`, `source`, `token`, `comment`, `flag`, `user_type`, " +
		" `ctime`, `mtime`) VALUES (?,?,?,?,?,?,?,?,?,sysdate(),sysdate())"

	_, err = tx.Exec(addSql, []interface{}{
		user.ID,
		user.Name,
		user.Password,
		user.Owner,
		user.Source,
		user.Token,
		user.Comment,
		0,
		user.Type,
	}...)

	if err != nil {
		return err
	}

	if err := createDefaultStrategy(tx, model.PrincipalUser, user.ID, user.Owner); err != nil {
		return store.Error(err)
	}

	if err := tx.Commit(); err != nil {
		logger.AuthScope().Errorf("[Store][User] add user tx commit err: %s", err.Error())
		return err
	}
	return nil
}

// UpdateUser 更新用户信息
func (u *userStore) UpdateUser(user *model.User) error {
	if user.ID == "" || user.Name == "" || user.Token == "" || user.Password == "" {
		return store.NewStatusError(store.EmptyParamsErr, fmt.Sprintf(
			"update user missing some params, id is %s, name is %s", user.ID, user.Name))
	}

	err := RetryTransaction("updateUser", func() error {
		return u.updateUser(user)
	})

	return store.Error(err)
}

func (u *userStore) updateUser(user *model.User) error {

	tx, err := u.master.Begin()
	if err != nil {
		return err
	}

	defer func() { _ = tx.Rollback() }()

	tokenEnable := 1
	if !user.TokenEnable {
		tokenEnable = 0
	}

	modifySql := "UPDATE user SET password = ?, token = ?, comment = ?, token_enable = ? WHERE id = ? AND flag = 0"

	_, err = tx.Exec(modifySql, []interface{}{
		user.Password,
		user.Token,
		user.Comment,
		tokenEnable,
		user.ID,
	}...)

	if err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		logger.AuthScope().Errorf("[Store][User] update user tx commit err: %s", err.Error())
		return err
	}

	return nil
}

// DeleteUser delete user by user id
func (u *userStore) DeleteUser(userId string) error {
	if userId == "" {
		return store.NewStatusError(store.EmptyParamsErr, "delete user id parameter missing")
	}

	err := RetryTransaction("deleteUser", func() error {
		return u.deleteUser(userId)
	})

	return store.Error(err)
}

func (u *userStore) deleteUser(id string) error {

	tx, err := u.master.Begin()
	if err != nil {
		return err
	}

	defer func() { _ = tx.Rollback() }()

	if _, err = tx.Exec("UPDATE user SET flag = 1 WHERE id = ?", []interface{}{
		id,
	}...); err != nil {
		return err
	}

	if _, err = tx.Exec("UPDATE user_group_relation SET flag = 1 WHERE user_id = ?", []interface{}{
		id,
	}...); err != nil {
		return err
	}

	if err := cleanLinkStrategy(tx, model.PrincipalUser, id); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		logger.AuthScope().Errorf("[Store][User] delete user tx commit err: %s", err.Error())
		return err
	}
	return nil
}

func (u *userStore) GetUser(id string) (*model.User, error) {

	var tokenEnable, userType int

	getSql := `
	SELECT u.id, u.name, u.password, u.owner, u.source
		, u.token, u.token_enable, u.user_type
	FROM user u
	WHERE u.flag = 0 AND u.name != 'polariadmin' 
		AND u.id = ? 
	`
	row := u.master.QueryRow(getSql, id)

	user := new(model.User)
	if err := row.Scan(&user.ID, &user.Name, &user.Password, &user.Owner, &user.Source,
		&user.Token, &tokenEnable, &userType); err != nil {
		switch err {
		case sql.ErrNoRows:
			return nil, nil
		default:
			return nil, store.Error(err)
		}
	}

	user.TokenEnable = (tokenEnable == 1)
	user.Type = model.UserRoleType(userType)

	return user, nil
}

// GetUserByName 根据用户名、owner 获取用户
func (u *userStore) GetUserByName(name, ownerId string) (*model.User, error) {
	getSql := `
	SELECT u.id, u.name, u.password, u.owner, u.source
		, u.token, u.token_enable, u.user_type
	FROM user u
	WHERE u.flag = 0
		AND u.name != 'polariadmin' 
		AND u.name = ?
		AND u.owner = ? 
	`

	user := new(model.User)
	var tokenEnable, userType int

	row := u.master.QueryRow(getSql, name, ownerId)
	if err := row.Scan(&user.ID, &user.Name, &user.Password, &user.Owner, &user.Source,
		&user.Token, &tokenEnable, &userType); err != nil {
		switch err {
		case sql.ErrNoRows:
			return nil, nil
		default:
			return nil, store.Error(err)
		}
	}

	user.TokenEnable = (tokenEnable == 1)
	user.Type = model.UserRoleType(userType)
	return user, nil

}

// GetUserByIDS 根据用户ID获取用户列表数据
func (u *userStore) GetUserByIDS(ids []string) ([]*model.User, error) {

	if len(ids) == 0 {
		return nil, nil
	}

	getSql := `
	SELECT u.id, u.name, u.password, u.owner, u.source
		, u.token, u.token_enable, u.user_type, UNIX_TIMESTAMP(u.ctime)
		, UNIX_TIMESTAMP(u.mtime), u.flag
	FROM user u
	WHERE u.flag = 0  AND u.name != 'polarisadmin' 
		AND u.id IN ( 
	`

	for i := range ids {
		getSql += " ? "
		if i != len(ids)-1 {
			getSql += ","
		}
	}
	getSql += ")"

	args := make([]interface{}, 0, 8)
	for index := range ids {
		args = append(args, ids[index])
	}

	rows, err := u.master.Query(getSql, args...)
	if err != nil {
		return nil, store.Error(err)
	}
	defer rows.Close()

	users := make([]*model.User, 0)
	for rows.Next() {
		user, err := fetchRown2User(rows)
		if err != nil {
			logger.AuthScope().Errorf("[Store][User] fetch user rows scan err: %s", err.Error())
			return nil, store.Error(err)
		}
		users = append(users, user)
	}

	return users, nil
}

// GetUsers 查询用户列表信息
func (u *userStore) GetUsers(filters map[string]string, offset uint32, limit uint32) (uint32, []*model.User, error) {

	if _, ok := filters["group_id"]; ok {
		return u.listGroupUsers(filters, offset, limit)
	}

	return u.listUsers(filters, offset, limit)

}

// listUsers 查询用户列表信息
func (u *userStore) listUsers(filters map[string]string, offset uint32, limit uint32) (uint32, []*model.User, error) {

	countSql := "SELECT COUNT(*) FROM user  WHERE flag = 0 "
	getSql := `
	SELECT id, name, password, owner, source
		, token, token_enable, user_type, UNIX_TIMESTAMP(ctime)
		, UNIX_TIMESTAMP(mtime), flag
	FROM user
	WHERE flag = 0  AND name != 'polarisadmin' 
	`

	args := make([]interface{}, 0)

	if len(filters) != 0 {
		for k, v := range filters {
			getSql += " AND "
			countSql += " AND "
			if k == NameAttribute {
				if utils.IsWildName(v) {
					getSql += (" " + k + " like ? ")
					countSql += (" " + k + " like ? ")
					args = append(args, v[:len(v)-1]+"%")
				} else {
					getSql += (" " + k + " = ? ")
					countSql += (" " + k + " = ? ")
					args = append(args, v)
				}
			} else if k == OwnerAttribute {
				getSql += " (id = ? OR owner = ?) "
				countSql += " (id = ? OR owner = ?) "
				args = append(args, v, v)
				continue
			} else {
				getSql += (" " + k + " = ? ")
				countSql += (" " + k + " = ? ")
				args = append(args, v)
			}
		}
	}

	logger.AuthScope().Debug("[Store][User] list user", zap.String("count sql", countSql), zap.Any("args", args))

	count, err := queryEntryCount(u.master, countSql, args)
	if err != nil {
		return 0, nil, store.Error(err)
	}

	getSql += " ORDER BY mtime LIMIT ? , ?"
	getArgs := append(args, offset, limit)

	users, err := u.collectUsers(u.master.Query, getSql, getArgs, logger.AuthScope())
	if err != nil {
		return 0, nil, err
	}
	return count, users, nil
}

// listGroupUsers 查询某个用户组下的用户信息
func (u *userStore) listGroupUsers(filters map[string]string, offset uint32, limit uint32) (uint32, []*model.User, error) {
	if _, ok := filters[GroupIDAttribute]; !ok {
		return 0, nil, store.NewStatusError(store.EmptyParamsErr, "group_id is missing")
	}
	filters["ug.group_id"] = filters[GroupIDAttribute]
	delete(filters, GroupIDAttribute)

	args := make([]interface{}, 0, len(filters))
	querySql := `
		SELECT u.id, name, password, owner, source
			, token, token_enable, user_type, UNIX_TIMESTAMP(u.ctime)
			, UNIX_TIMESTAMP(u.mtime), u.flag
		FROM user_group_relation ug
			LEFT JOIN user u ON ug.user_id = u.id AND u.flag = 0 AND ug.flag = 0
		WHERE 1=1 AND u.name != 'polarisadmin' 
	`
	countSql := `
		SELECT COUNT(*)
		FROM user_group_relation ug
			LEFT JOIN user u ON ug.user_id = u.id AND u.flag = 0 AND ug.flag = 0
		WHERE 1=1 AND u.name != 'polarisadmin' 
	`

	for k, v := range filters {
		if newK, ok := userLinkGroupAttributeMapping[k]; ok {
			k = newK
		}
		if utils.IsWildName(v) {
			querySql += " AND " + k + " like ?"
			countSql += " AND " + k + " like ?"
			args = append(args, v[:len(v)-1]+"%")
		} else {
			querySql += " AND " + k + " = ?"
			countSql += " AND " + k + " = ?"
			args = append(args, v)
		}
	}

	count, err := queryEntryCount(u.slave, countSql, args)
	logger.AuthScope().Debug("count list user", zap.String("sql", countSql), zap.Any("args", args))

	if err != nil {
		return 0, nil, err
	}

	querySql += " ORDER BY u.mtime LIMIT ? , ?"
	args = append(args, offset, limit)

	users, err := u.collectUsers(u.master.Query, querySql, args, logger.AuthScope())
	if err != nil {
		return 0, nil, err
	}

	return count, users, nil
}

// GetUsersForCache 获取用户信息，主要是为了 Cache 使用的
func (u *userStore) GetUsersForCache(mtime time.Time, firstUpdate bool) ([]*model.User, error) {

	args := make([]interface{}, 0)

	querySql := `
	SELECT u.id, u.name, u.password, u.owner, u.source
		, u.token, u.token_enable, user_type, UNIX_TIMESTAMP(u.ctime)
		, UNIX_TIMESTAMP(u.mtime), u.flag
	FROM user u 
	`

	if !firstUpdate {
		querySql += " WHERE u.mtime >= ? "
		args = append(args, commontime.Time2String(mtime))
	}

	users, err := u.collectUsers(u.master.Query, querySql, args, logger.CacheScope())
	if err != nil {
		return nil, err
	}

	return users, nil
}

// collectUsers 通用的查询用户列表的操作
func (u *userStore) collectUsers(handler QueryHandler, querySql string, args []interface{}, scope *logger.Scope) ([]*model.User, error) {

	scope.Debug("[Store][User] list user ", zap.String("query sql", querySql), zap.Any("args", args))
	rows, err := u.master.Query(querySql, args...)
	if err != nil {
		return nil, store.Error(err)
	}
	defer rows.Close()

	users := make([]*model.User, 0)
	for rows.Next() {
		user, err := fetchRown2User(rows)
		if err != nil {
			scope.Errorf("[Store][User] fetch user rows scan err: %s", err.Error())
			return nil, store.Error(err)
		}
		users = append(users, user)
	}

	return users, nil
}

func createDefaultStrategy(tx *BaseTx, role model.PrincipalType, id, owner string) error {
	// 创建该用户的默认权限策略
	strategy := &model.StrategyDetail{
		ID:        utils.NewUUID(),
		Name:      model.BuildDefaultStrategyName(id, role),
		Action:    api.AuthAction_READ_WRITE.String(),
		Comment:   "default user auth_strategy",
		Default:   true,
		Owner:     owner,
		Revision:  utils.NewUUID(),
		Resources: []model.StrategyResource{},
		Valid:     true,
	}

	// 保存策略主信息
	saveMainSql := "INSERT INTO auth_strategy(`id`, `name`, `action`, `owner`, `comment`, `flag`, " +
		" `default`, `revision`) VALUES (?,?,?,?,?,?,?,?)"
	_, err := tx.Exec(saveMainSql, []interface{}{strategy.ID, strategy.Name, strategy.Action, strategy.Owner, strategy.Comment,
		0, strategy.Default, strategy.Revision}...)

	if err != nil {
		return err
	}

	savePrincipalSql := "INSERT INTO auth_principal(`strategy_id`, `principal_id`, `principal_role`) VALUES (?,?,?)"
	_, err = tx.Exec(savePrincipalSql, []interface{}{strategy.ID, id, role}...)
	if err != nil {
		return err
	}

	return nil
}

// cleanLinkStrategy 清理与自己相关联的鉴权信息
func cleanLinkStrategy(tx *BaseTx, role model.PrincipalType, id string) error {

	var err error

	// 清理默认策略
	if _, err = tx.Exec("UPDATE auth_strategy SET flag = 1 WHERE name = ?", []interface{}{
		model.BuildDefaultStrategyName(id, model.PrincipalUserGroup),
	}...); err != nil {
		return err
	}

	// 清理默认策略对应的所有鉴权关联资源
	if _, err = tx.Exec("DELETE FROM auth_strategy_resource WHERE strategy_id = (SELECT id FROM auth_strategy WHERE name = ?)", []interface{}{
		model.BuildDefaultStrategyName(id, model.PrincipalUserGroup),
	}...); err != nil {
		return err
	}

	// 清理默认鉴权策略涉及的所有关联人员信息
	if _, err = tx.Exec("DELETE FROM auth_principal WHERE strategy_id = (SELECT id FROM auth_strategy WHERE name = ?)", []interface{}{
		model.BuildDefaultStrategyName(id, model.PrincipalUserGroup),
	}...); err != nil {
		return err
	}

	// 清理所在的所有鉴权principal
	if _, err = tx.Exec("DELETE FROM auth_principal WHERE principa_id = ? AND principal_role = ?", []interface{}{
		id, model.PrincipalUserGroup,
	}...); err != nil {
		return err
	}

	return nil
}

func fetchRown2User(rows *sql.Rows) (*model.User, error) {
	var ctime, mtime int64
	var flag, tokenEnable, userType int
	user := new(model.User)
	err := rows.Scan(&user.ID, &user.Name, &user.Password, &user.Owner, &user.Source, &user.Token,
		&tokenEnable, &userType, &ctime, &mtime, &flag)

	if err != nil {
		return nil, err
	}

	user.Valid = flag == 0
	user.TokenEnable = tokenEnable == 1
	user.CreateTime = time.Unix(ctime, 0)
	user.ModifyTime = time.Unix(mtime, 0)
	user.Type = model.UserRoleType(userType)

	return user, nil
}

func (u *userStore) cleanInValidUser(name string) error {
	logger.AuthScope().Infof("[Store][User] clean user(%s)", name)
	str := "delete from user where name = ? and flag = 1"
	_, err := u.master.Exec(str, name)
	if err != nil {
		logger.AuthScope().Errorf("[Store][User] clean user(%s) err: %s", name, err.Error())
		return err
	}

	return nil
}

func checkAffectedRows(label string, result sql.Result, count int64) error {
	n, err := result.RowsAffected()
	if err != nil {
		logger.AuthScope().Errorf("[Store][%s] get rows affected err: %s", label, err.Error())
		return err
	}

	if n == count {
		return nil
	}
	logger.AuthScope().Errorf("[Store][%s] get rows affected result(%d) is not match expect(%d)", label, n, count)
	return store.NewStatusError(store.AffectedRowsNotMatch, "affected rows not match")
}
