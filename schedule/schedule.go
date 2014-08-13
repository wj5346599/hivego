//调度模块，负责从元数据库读取并解析调度信息。
//将需要执行的任务发送给执行模块，并读取返回信息。
package schedule

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/Sirupsen/logrus"
	"time"
)

var (
	g *GlobalConfigStruct
)

//GlobalConfigStruct结构中定义了程序中的一些配置信息
type GlobalConfigStruct struct { // {{{
	L           *logrus.Logger      //log对象
	HiveConn    *sql.DB             //元数据库链接
	LogConn     *sql.DB             //日志数据库链接
	Port        string              //Schedule与Worker模块通信端口
	ExecScdChan chan *ExecSchedule  //执行链信息
	Tasks       map[string]*Task    //全局Task列表
	ExecTasks   map[int64]*ExecTask //全局ExecTask列表
	Schedules   *ScheduleManager    //包含全部Schedule列表的结构
} // }}}

//返回GlobalConfigStruct的默认值。
func DefaultGlobal() *GlobalConfigStruct { // {{{
	sc := &GlobalConfigStruct{}
	sc.L = logrus.New()
	sc.L.Formatter = new(logrus.TextFormatter) // default
	sc.L.Level = logrus.Info
	sc.Port = ":3128"
	sc.ExecScdChan = make(chan *ExecSchedule)
	sc.ExecTasks = make(map[int64]*ExecTask)
	sc.Tasks = make(map[string]*Task)
	sc.Schedules = &ScheduleManager{Global: sc}
	return sc
} // }}}

//ScheduleManager通过成员ScheduleList持有全部的Schedule。
//并提供获取、增加、删除以及启动、停止Schedule的功能。
type ScheduleManager struct { // {{{
	ScheduleList []*Schedule         //全部的调度列表
	Global       *GlobalConfigStruct //配置信息
} // }}}

//初始化ScheduleList，设置全局变量g
func (sl *ScheduleManager) InitScheduleList() { // {{{
	var err error

	g = sl.Global
	//从元数据库读取调度信息,初始化调度列表
	sl.ScheduleList, err = getAllSchedules()
	if err != nil {
		e := fmt.Sprintf("[sl.InitScheduleList] init scheduleList error %s.\n", err.Error())
		g.L.Fatalln(e)
	}

} // }}}

//遍历列表中的Schedule并启动它的Timer方法。
func (sl *ScheduleManager) StartListener() { // {{{
	for _, scd := range sl.ScheduleList {
		//从元数据库初始化调度链信息
		err := scd.InitSchedule()
		if err != nil {
			e := fmt.Sprintf("[sl.StartListener] init schedule [%d] error %s.\n", scd.Id, err.Error())
			g.L.Warningln(e)
			return
		}

		//启动监听，按时启动Schedule
		go scd.Timer()
	}

} // }}}

//启动指定的Schedule，从ScheduleList中获取到指定id的Schedule后，从元数据库获取
//Schedule的信息初始化一下调度链，然后调用它自身的Timer方法，启动监听。
//失败返回error信息。
func (sl *ScheduleManager) StartScheduleById(id int64) error { // {{{
	s := sl.GetScheduleById(id)
	if s == nil {
		e := fmt.Sprintf("[sl.StartScheduleById] start schedule. not found schedule by id %d\n", id)
		g.L.Warningln(e)
		return errors.New(e)
	}

	//从元数据库初始化调度链信息
	err := s.InitSchedule()
	if err != nil {
		e := fmt.Sprintf("[sl.StartScheduleById] init schedule [%d] error %s.\n", id, err.Error())
		g.L.Warningln(e)
		return errors.New(e)
	}

	//启动监听，按时启动Schedule
	go s.Timer()

	return nil
} // }}}

//查找当前ScheduleList列表中指定id的Schedule，并返回。
//查不到返回nil
func (sl *ScheduleManager) GetScheduleById(id int64) *Schedule { // {{{
	for _, s := range sl.ScheduleList {
		if s.Id == id {
			return s
		}
	}
	return nil
} // }}}

//从当前ScheduleList列表中移除指定id的Schedule。
//完成后，调用Schedule自身的Delete方法，删除其中的Job、Task信息并做持久化操作。
//失败返回error信息
func (sl *ScheduleManager) DeleteSchedule(id int64) error { // {{{
	i := -1
	for k, ss := range sl.ScheduleList {
		if ss.Id == id {
			i = k
		}
	}

	if i == -1 {
		e := fmt.Sprintf("[sl.DeleteSchedule] delete error. not found schedule by id %d\n", id)
		g.L.Warningln(e)
		return errors.New(e)
	}

	s := sl.ScheduleList[i]
	sl.ScheduleList = append(sl.ScheduleList[0:i], sl.ScheduleList[i+1:]...)

	err := s.Delete()
	if err != nil {
		e := fmt.Sprintf("[sl.DeleteSchedule] delete schedule [%d %s] error. %s\n", id, s.Name, err.Error())
		g.L.Warningln(e)
		return errors.New(e)
	}

	return nil
} // }}}

//调度信息结构
type Schedule struct { // {{{
	Id           int64           //调度ID
	Name         string          //调度名称
	Count        int8            //调度次数
	Cyc          string          //调度周期
	StartSecond  []time.Duration //启动时间
	StartMonth   []int           //启动月份
	NextStart    time.Time       //下次启动时间
	TimeOut      int64           //最大执行时间
	JobId        int64           //作业ID
	Job          *Job            //作业
	Jobs         []*Job          //作业列表
	Tasks        []*Task         `json:"-"` //任务列表
	Desc         string          //调度说明
	JobCnt       int64           //调度中作业数量
	TaskCnt      int64           //调度中任务数量
	CreateUserId int64           //创建人
	CreateTime   time.Time       //创人
	ModifyUserId int64           //修改人
	ModifyTime   time.Time       //修改时间
} // }}}

//按时启动Schedule，Timer中会根据Schedule的周期以及启动时间计算下次
//启动的时间，并依据此设置一个定时器按时唤醒，Schedule唤醒后，会重新
//从元数据库初始化一下信息，生成执行结构ExecSchedule，执行其Run方法
func (s *Schedule) Timer() { // {{{
	//获取距启动的时间（秒）
	countDown, err := getCountDown(s.Cyc, s.StartMonth, s.StartSecond)
	if err != nil {
		e := fmt.Sprintf("[s.Timer] get schedule [%d %s] start time error %s.\n", s.Id, s.Name, err.Error())
		g.L.Warningln(e)
		return
	}

	s.NextStart = time.Now().Add(countDown)
	select {
	case <-time.After(countDown):
		//从元数据库初始化调度链信息
		err := s.InitSchedule()
		if err != nil {
			e := fmt.Sprintf("[s.Timer] init schedule [%d] error %s.\n", s.Id, err.Error())
			g.L.Warningln(e)
			return
		}

		l := fmt.Sprintf("[s.Timer] schedule [%d %s] is start.\n", s.Id, s.Name)
		g.L.Print(l)

		//构建执行结构链
		es, err := NewExecSchedule(s)
		if err != nil {
			e := fmt.Sprintf("[s.Timer] create Exec schedule [%d %s] error %s.\n", s.Id, s.Name, err.Error())
			g.L.Warningln(e)
			return
		}

		//启动线程执行调度任务
		go es.Run()
	}
	return
} // }}}

//从元数据库初始化Schedule结构，先从元数据库获取Schedule的信息，完成后
//根据其中的Jobid继续从元数据库读取job信息，并初始化。完成后继续初始化下级Job，
//同时将初始化完成的Job和Task添加到Schedule的Jobs、Tasks成员中。
func (s *Schedule) InitSchedule() error { // {{{
	ts, err := getSchedule(s.Id)
	if err != nil {
		e := fmt.Sprintf("[s.InitSchedule] get schedule [%d] error %s.\n", s.Id, err.Error())
		g.L.Warningln(e)
		return errors.New(e)
	}
	s.Name, s.Count, s.Cyc, s.Desc = ts.Name, ts.Count, ts.Cyc, ts.Desc
	s.StartSecond, s.TimeOut, s.JobId = ts.StartSecond, ts.TimeOut, ts.JobId

	if tj, err := getJob(s.JobId); tj != nil {
		tj.ScheduleId, tj.ScheduleCyc = s.Id, s.Cyc
		if err = tj.initJob(); err != nil {
			e := fmt.Sprintf("[s.InitSchedule] init job [%d] error %s.\n", s.JobId, err.Error())
			g.L.Warningln(e)
			return errors.New(e)
		}
		s.Job = tj
		s.Jobs, s.Tasks = make([]*Job, 0), make([]*Task, 0)
		s.JobCnt, s.TaskCnt = 0, 0
		for j := s.Job; j != nil; {
			s.Jobs = append(s.Jobs, j)
			s.JobCnt++
			s.TaskCnt += j.TaskCnt
			for _, t := range j.Tasks {
				s.addTaskList(t)
			}
			j = j.NextJob
		}
	} else if err != nil {
		e := fmt.Sprintf("[s.InitSchedule] get job [%d] error %s.\n", s.JobId, err.Error())
		g.L.Warningln(e)
		return errors.New(e)
	}
	return nil
} // }}}

//addTaskList将传入的*Task添加到*Schedule.Tasks中
func (s *Schedule) addTaskList(t *Task) { // {{{
	s.Tasks = append(s.Tasks, t)
} // }}}

//GetTaskById根据传入的id查找Tasks中对应的Task，没有则返回nil。
func (s *Schedule) GetTaskById(id int64) *Task { // {{{
	for _, v := range s.Tasks {
		if v.Id == id {
			return v
		}
	}
	return nil
} // }}}

//在调度中添加一个Job，AddJob会接收传入的Job类型的参数，并调用它的
//Add()方法进行持久化操作。成功后把它添加到调度链中，添加时若调度
//下无Job则将Job直接添加到调度中，否则添加到调度中的任务链末端。
func (s *Schedule) AddJob(job *Job) (err error) { // {{{
	if err = job.add(); err == nil {
		if len(s.Jobs) == 0 {
			s.JobId, s.Job = job.Id, job
			if err = s.update(); err != nil {
				e := fmt.Sprintf("[s.AddJob] update schedule [%d] error %s.\n", s.Id, err.Error())
				g.L.Warningln(e)
				return errors.New(e)
			}
		} else {
			j := s.Jobs[len(s.Jobs)-1]
			j.NextJob, j.NextJobId, job.PreJob = job, job.Id, j
			if err = j.update(); err != nil {
				e := fmt.Sprintf("[s.AddJob] update job [%d] error %s.\n", job.Id, err.Error())
				g.L.Warningln(e)
				return errors.New(e)
			}
		}
		s.Jobs = append(s.Jobs, job)
		s.JobCnt += 1
	}
	return err
} // }}}

//DeleteJob删除调度中最后一个Job，它会接收传入的Job Id，并查看是否
//调度中最后一个Job，是，检查Job下有无Task，无，则执行删除操作，完成
//后，将该Job的前一个Job的nextJob指针置0，更新调度信息。
//出错或不符条件则返回error信息
func (s *Schedule) DeleteJob(id int64) (err error) { // {{{
	if j := s.GetJobById(id); j != nil && j.TaskCnt == 0 && j.NextJobId == 0 {

		if pj := s.GetJobById(j.PreJobId); pj != nil {

			pj.NextJob, pj.NextJobId = nil, 0
			if err = pj.update(); err != nil {
				e := fmt.Sprintf("[s.DeleteJob] update job [%d] to schedule [%d] error %s.\n", j.Id, s.Id, err.Error())
				g.L.Warningln(e)
				return errors.New(e)
			}
		}

		if len(s.Jobs) == 1 {
			s.Jobs, s.Job, s.JobId = make([]*Job, 0), nil, 0
			if err = s.update(); err != nil {
				e := fmt.Sprintf("[s.DeleteJob] update schedule [%d] error %s.\n", s.Id, err.Error())
				g.L.Warningln(e)
				return errors.New(e)
			}
		} else {
			s.Jobs = s.Jobs[0 : len(s.Jobs)-1]
		}

		s.JobCnt--
		err = j.deleteJob()
		if err != nil {
			e := fmt.Sprintf("[s.DeleteJob] delete job [%d] error %s.\n", j.Id, err.Error())
			g.L.Warningln(e)
			return errors.New(e)
		}
	} else {
		e := fmt.Sprintf("[s.DeleteJob] not found job by id %d", id)
		g.L.Warningln(e)
		err = errors.New(e)
	}
	return err
} // }}}

//UpdateJob用来在调度中添加一个Job
//UpdateJob会接收传入的Job类型的参数，修改调度中对应的Job信息，完成后
//调用Job自身的update方法进行持久化操作。
func (s *Schedule) UpdateJob(job *Job) (err error) { // {{{
	if j := s.GetJobById(job.Id); j != nil {
		j.Name, j.Desc = job.Name, job.Desc
		j.ModifyTime, j.ModifyUserId = time.Now(), job.ModifyUserId
		err = j.update()
		if err != nil {
			e := fmt.Sprintf("[s.UpdateJob] update job [%d] error %s.\n", j.Id, err.Error())
			g.L.Warningln(e)
			return errors.New(e)
		}
	} else {
		e := fmt.Sprintf("[s.UpdateJob] not found job by id %d", job.Id)
		g.L.Warningln(e)
		err = errors.New(e)
	}
	return err
} // }}}

//UpdateSchedule方法会将传入参数的信息更新到Schedule结构并持久化到数据库中
//在持久化之前会调用addStart方法将启动列表持久化
func (s *Schedule) UpdateSchedule(scd *Schedule) (err error) { // {{{
	s.Name, s.Desc, s.Cyc, s.StartMonth = scd.Name, scd.Desc, scd.Cyc, scd.StartMonth
	s.StartSecond, s.ModifyTime, s.ModifyUserId = scd.StartSecond, time.Now(), scd.ModifyUserId
	if err = s.AddStart(); err != nil {
		e := fmt.Sprintf("[s.UpdateSchedule] addstart error %s.\n", err.Error())
		g.L.Warningln(e)
		return errors.New(e)
	}

	if err = s.update(); err != nil {
		e := fmt.Sprintf("[s.UpdateSchedule] update schedule [%d] error %s.\n", s.Id, err.Error())
		g.L.Warningln(e)
		return errors.New(e)
	}

	return err
} // }}}

//DeleteTask方法用来删除指定id的Task。首先会根据传入参数在Schedule的Tasks列
//表中查出对应的Task。然后将其从Tasks列表中去除，将其从所属Job中去除，调用
//Task的Delete方法删除Task的依赖关系，完成后删除元数据库的信息。
//没找到对应Task或删除失败，返回error信息。
func (s *Schedule) DeleteTask(id int64) (err error) { // {{{
	i := -1
	for k, task := range s.Tasks {
		if task.Id == id {
			i = k
		}

		if _, ok := task.RelTasks[string(id)]; ok {
			err = task.DeleteRelTask(id)
			if err != nil {
				e := fmt.Sprintf("[s.DeleteTask] schedule [%d] DeleteRelTask taskid reltaskid [%d %d] error %s.\n", s.Id,
					task.Id, id, err.Error())
				g.L.Warningln(e)
				return errors.New(e)
			}
		}
	}
	if i == -1 {
		e := fmt.Sprintf("[s.DeleteTask] not found task by id %d", id)
		g.L.Warningln(e)
		return errors.New(e)
	}

	t := s.Tasks[i]
	s.Tasks = append(s.Tasks[0:i], s.Tasks[i+1:]...)
	s.TaskCnt--

	j := s.GetJobById(t.JobId)
	if err = j.DeleteTask(t.Id); err != nil {
		e := fmt.Sprintf("[s.DeleteTask] DeleteTask error %s", err.Error())
		g.L.Warningln(e)
		return errors.New(e)
	}

	err = t.Delete()
	if err != nil {
		e := fmt.Sprintf("[s.DeleteTask] schedule [%d] Delete error %s.\n", err.Error())
		g.L.Warningln(e)
		return errors.New(e)
	}

	return err
} // }}}

//GetJobById遍历Jobs列表，返回调度中指定Id的Job，若没找到返回nil
func (s *Schedule) GetJobById(Id int64) *Job { // {{{
	for _, j := range s.Jobs {
		if j.Id == Id {
			return j
		}
	}
	return nil
} // }}}

//Delete方法删除Schedule下的Job、Task信息并持久化。
func (s *Schedule) Delete() error { // {{{
	for _, t := range s.Tasks {
		err := s.DeleteTask(t.Id)
		if err != nil {
			e := fmt.Sprintf("[s.Delete] DeleteTask [%d] error %s.\n", t.Id, err.Error())
			g.L.Warningln(e)
			return errors.New(e)
		}
	}

	for _, j := range s.Jobs {
		err := s.DeleteJob(j.Id)
		if err != nil {
			e := fmt.Sprintf("[s.Delete] DeleteJob [%d] error %s.\n", j.Id, err.Error())
			g.L.Warningln(e)
			return errors.New(e)
		}
	}

	err := s.delStart()
	if err != nil {
		e := fmt.Sprintf("[s.Delete] delStart error %s.\n", err.Error())
		g.L.Warningln(e)
		return errors.New(e)
	}

	err = s.deleteSchedule()
	if err != nil {
		e := fmt.Sprintf("[s.Delete] deleteSchedule [%d] error %s.\n", s.Id, err.Error())
		g.L.Warningln(e)
		return errors.New(e)
	}
	return nil
} // }}}

//addStart将Schedule的启动列表持久化到数据库
//添加前先调用delStart方法将Schedule中的原有启动列表清空
//需要注意的是：内存中的启动列表单位为纳秒，存储前需要转成秒
//若成功则开始添加，失败返回err信息
func (s *Schedule) AddStart() (err error) { // {{{
	if err = s.delStart(); err == nil {
		for i, st := range s.StartSecond {
			err = s.addStart(time.Duration(st)/time.Second, s.StartMonth[i])
			if err != nil {
				e := fmt.Sprintf("[s.AddStart] error %s.\n", err.Error())
				g.L.Warningln(e)
				return errors.New(e)
			}
		}
	} else {
		e := fmt.Sprintf("[s.AddStart] delStart error %s.\n", err.Error())
		g.L.Warningln(e)
	}
	return err
} // }}}

//启动时间排序
//算法选择排序
func (s *Schedule) sortStart() { // {{{
	var i, j, k int

	for i = 0; i < len(s.StartMonth); i++ {
		k = i

		for j = i + 1; j < len(s.StartMonth); j++ {
			if s.StartMonth[j] < s.StartMonth[k] {
				k = j
			} else if (s.StartMonth[j] == s.StartMonth[k]) && (s.StartSecond[j] < s.StartSecond[k]) {
				k = j
			}
		}

		if k != i {
			s.StartMonth[k], s.StartMonth[i] = s.StartMonth[i], s.StartMonth[k]
			s.StartSecond[k], s.StartSecond[i] = s.StartSecond[i], s.StartSecond[k]
		}

	}

} // }}}
